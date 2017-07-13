package main

import (
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
)

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("Got a publish request!")

	var obj Object
	var err error
	if obj, err = decodePayload(r); err != nil {
		fmt.Println("Failed to parse request.")
		return
	}

	fmt.Printf("Bucket:\t%s\n", obj.Bucket)
	fmt.Printf("Key:\t%s\n", obj.Key)

	publisherChannel <- obj
	w.WriteHeader(200)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	var obj StatusRequest

	vars := mux.Vars(r)

	obj.Key = string(vars["id"])

	statusChannel <- obj
	obj = <-statusChannel

	if obj.Status == "done" {
		w.Write([]byte("done"))
	} else if obj.Status == "publishing" {
		w.Write([]byte("publishing"))
	} else {
		w.Write([]byte("unknown"))
	}
}