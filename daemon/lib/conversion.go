package lib

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"

	da "github.com/cvmfs/docker-graphdriver/daemon/docker-api"

	"github.com/docker/distribution"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	log "github.com/sirupsen/logrus"
)

var subDirInsideRepo = ".layers"

func ConvertWish(wish WishFriendly, convertAgain, forceDownload, convertSingularity bool) (err error) {

	outputImage, err := GetImageById(wish.OutputId)
	if err != nil {
		return
	}
	password, err := GetUserPassword(outputImage.User, outputImage.Registry)
	if err != nil {
		return
	}
	inputImage, err := GetImageById(wish.InputId)
	if err != nil {
		return
	}
	manifest, err := inputImage.GetManifest()
	if err != nil {
		return
	}

	alreadyConverted := AlreadyConverted(wish.CvmfsRepo, inputImage, manifest.Config.Digest)
	Log().WithFields(log.Fields{"alreadyConverted": alreadyConverted}).Info(
		"Already converted the image, skipping.")

	if alreadyConverted && convertAgain == false {
		Log().Info("Already converted the image, skipping.")
		return nil
	}
	layersChanell := make(chan downloadedLayer, 3)
	manifestChanell := make(chan string, 1)
	stopGettingLayers := make(chan bool, 1)
	noErrorInConversion := make(chan bool, 1)

	type LayerRepoLocation struct {
		Digest   string
		Location string //location does NOT need the prefix `/cvmfs`
	}
	layerRepoLocationChan := make(chan LayerRepoLocation, 3)
	layerMetadataLocationChan := make(chan string, 3)
	go func() {
		noErrors := true
		var wg sync.WaitGroup
		defer func() {
			wg.Wait()
			close(layerRepoLocationChan)
			close(layerMetadataLocationChan)
		}()
		defer func() {
			noErrorInConversion <- noErrors
			stopGettingLayers <- true
			close(stopGettingLayers)
		}()
		cleanup := func(location string) {
			Log().Info("Running clean up function deleting the last layer.")

			err := ExecCommand("cvmfs_server", "abort", "-f", wish.CvmfsRepo).Start()
			if err != nil {
				LogE(err).Warning("Error in the abort command inside the cleanup function, this warning is usually normal")
			}

			err = ExecCommand("cvmfs_server", "ingest", "--delete", location, wish.CvmfsRepo).Start()
			if err != nil {
				LogE(err).Error("Error in the cleanup command")
			}
		}
		layerRepoLocationRoot := filepath.Join("/", "cvmfs", wish.CvmfsRepo)
		for layer := range layersChanell {

			Log().WithFields(log.Fields{"layer": layer.Name}).Info("Start Ingesting the file into CVMFS")
			layerDigest := strings.Split(layer.Name, ":")[1]
			layerRoot := filepath.Join(subDirInsideRepo, layerDigest[0:2], layerDigest)
			layerLocation := filepath.Join(layerRoot, "layerfs")
			layerMetadata := filepath.Join(layerRoot, ".metadata")

			var pathExists bool
			layerPath := filepath.Join("/", "cvmfs", wish.CvmfsRepo, layerLocation)

			if _, err := os.Stat(layerPath); os.IsNotExist(err) {
				pathExists = false
			} else {
				pathExists = true
			}

			// need to run this into a goroutine to avoid a deadlock
			wg.Add(1)
			go func(layerName, layerLocation, layerMetadata string) {
				layerRepoLocationChan <- LayerRepoLocation{
					Digest:   layerName,
					Location: layerLocation}
				layerMetadataLocationChan <- layerMetadata
				wg.Done()
			}(layer.Name, filepath.Join(layerRepoLocationRoot, layerLocation), layerMetadata)

			if pathExists == false || forceDownload {
				err = ExecCommand("cvmfs_server", "ingest", "-t", layer.Path, "-b", layerLocation, wish.CvmfsRepo).Start()

				if err != nil {
					LogE(err).WithFields(log.Fields{"layer": layer.Name}).Error("Some error in ingest the layer")
					noErrors = false
					cleanup(layerLocation)
					return
				}
				Log().WithFields(log.Fields{"layer": layer.Name}).Info("Finish Ingesting the file")
			} else {
				Log().WithFields(log.Fields{"layer": layer.Name}).Info("Skipping ingestion of layer, already exists")
			}
			os.Remove(layer.Path)
		}
		Log().Info("Finished pushing the layers into CVMFS")
	}()
	// we create a temp directory for all the files needed, when this function finish we can remove the temp directory cleaning up
	tmpDir, err := ioutil.TempDir("", "conversion")
	if err != nil {
		LogE(err).Error("Error in creating a temporary direcotry for all the files")
		return
	}
	defer os.RemoveAll(tmpDir)

	// this wil start to feed the above goroutine by writing into layersChanell
	err = inputImage.GetLayers(layersChanell, manifestChanell, stopGettingLayers, tmpDir)

	var singularity Singularity
	if convertSingularity {
		singularity, err = inputImage.DownloadSingularityDirectory(tmpDir)
		if err != nil {
			LogE(err).Error("Error in dowloading the singularity image")
			return
		}
		defer os.RemoveAll(singularity.TempDirectory)
	}
	changes, _ := inputImage.GetChanges()

	var wg sync.WaitGroup

	layerLocations := make(map[string]string)
	wg.Add(1)
	go func() {
		for layerLocation := range layerRepoLocationChan {
			layerLocations[layerLocation.Digest] = layerLocation.Location
		}
		wg.Done()
	}()

	var layerMetadaLocations []string
	wg.Add(1)
	go func() {
		for layerMetadaLocation := range layerMetadataLocationChan {
			layerMetadaLocations = append(layerMetadaLocations, layerMetadaLocation)
		}
		wg.Done()
	}()
	wg.Wait()

	thin, err := da.MakeThinImage(manifest, layerLocations, inputImage.WholeName())
	if err != nil {
		return
	}

	thinJson, err := json.MarshalIndent(thin, "", "  ")
	if err != nil {
		return
	}
	fmt.Println(string(thinJson))
	var imageTar bytes.Buffer
	tarFile := tar.NewWriter(&imageTar)
	header := &tar.Header{Name: "thin.json", Mode: 0644, Size: int64(len(thinJson))}
	err = tarFile.WriteHeader(header)
	if err != nil {
		return
	}
	_, err = tarFile.Write(thinJson)
	if err != nil {
		return
	}
	err = tarFile.Close()
	if err != nil {
		return
	}

	dockerClient, err := client.NewClientWithOpts(client.WithVersion("1.19"))
	if err != nil {
		return
	}

	image := types.ImageImportSource{
		Source:     bytes.NewBuffer(imageTar.Bytes()),
		SourceName: "-",
	}
	importOptions := types.ImageImportOptions{
		Tag:     outputImage.Tag,
		Message: "",
		Changes: changes,
	}
	importResult, err := dockerClient.ImageImport(
		context.Background(),
		image,
		outputImage.GetSimpleName(),
		importOptions)
	if err != nil {
		LogE(err).Error("Error in image import")
		return
	}
	defer importResult.Close()
	Log().Info("Created the image in the local docker daemon")

	// is necessary this mechanism to pass the authentication to the
	// dockers even if the documentation says otherwise
	authStruct := struct {
		Username string
		Password string
	}{
		Username: outputImage.User,
		Password: password,
	}
	authBytes, _ := json.Marshal(authStruct)
	authCredential := base64.StdEncoding.EncodeToString(authBytes)
	pushOptions := types.ImagePushOptions{
		RegistryAuth: authCredential,
	}

	res, err := dockerClient.ImagePush(
		context.Background(),
		outputImage.GetSimpleName(),
		pushOptions)
	if err != nil {
		return
	}
	// here is possible to use the result of the above ReadAll to have
	// informantion about the status of the upload.
	_, err = ioutil.ReadAll(res)
	if err != nil {
		return
	}
	Log().Info("Finish pushing the image to the registry")
	// we wait for the goroutines to finish
	// and if there was no error we add everything to the converted table
	noErrorInConversionValue := <-noErrorInConversion

	// here we can launch the ingestion for the singularity image
	if convertSingularity {
		err = singularity.IngestIntoCVMFS(wish.CvmfsRepo)
		if err != nil {
			LogE(err).Error("Error in ingesting the singularity image into the CVMFS repository")
			noErrorInConversionValue = false
		}
	}

	err = SaveLayersBacklink(wish.CvmfsRepo, inputImage, layerMetadaLocations)
	if err != nil {
		LogE(err).Error("Error in saving the backlinks")
		noErrorInConversionValue = false
	}

	if noErrorInConversionValue {
		manifestPath := filepath.Join(".metadata", inputImage.GetSimpleName(), "manifest.json")
		errIng := IngestIntoCVMFS(wish.CvmfsRepo, manifestPath, <-manifestChanell)
		if err != nil {
			LogE(errIng).Error("Error in storing the manifest in the repository")
		}
		errConv := AddConverted(wish.Id, manifest)
		if err != nil && convertAgain == false {
			LogE(errConv).Error("Error in storing the conversion in the database")
		}
		if errIng == nil && errConv == nil {
			Log().Info("Conversion completed")
		}
		return
	} else {
		Log().Warn("Some error during the conversion, we are not storing it into the database")
		return
	}
}

func AlreadyConverted(CVMFSRepo string, img Image, reference string) bool {
	path := filepath.Join("/", "cvmfs", CVMFSRepo, ".metadata", img.GetSimpleName(), "manifest.json")

	// from https://github.com/moby/moby/blob/8e610b2b55bfd1bfa9436ab110d311f5e8a74dcb/image/tarexport/tarexport.go#L18
	type manifestItem struct {
		Config       string
		RepoTags     []string
		Layers       []string
		Parent       image.ID                                 `json:",omitempty"`
		LayerSources map[layer.DiffID]distribution.Descriptor `json:",omitempty"`
	}

	fmt.Println(path)
	manifestStat, err := os.Stat(path)
	if os.IsNotExist(err) {
		Log().Info("Manifest not existing")
		return false
	}
	if !manifestStat.Mode().IsRegular() {
		Log().Info("Manifest not a regular file")
		return false
	}

	manifestFile, err := os.Open(path)
	if err != nil {
		Log().Info("Error in opening the manifest")
		return false
	}
	defer manifestFile.Close()

	bytes, _ := ioutil.ReadAll(manifestFile)

	var manifest da.Manifest
	err = json.Unmarshal(bytes, &manifest)
	if err != nil {
		LogE(err).Warning("Error in unmarshaling the manifest")
		return false
	}
	fmt.Printf("%s == %s\n", manifest.Config.Digest, reference)
	if manifest.Config.Digest == reference {
		return true
	}
	return false
}
