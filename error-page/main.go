package main

import (
	"net/http"
	"os"
)

type errorPage struct {
	runVersion string
}

func (ep *errorPage) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/api/error_page" {
		rw.WriteHeader(http.StatusNotFound)
		return
	}
	rw.WriteHeader(http.StatusOK)

	var data []byte

	if ep.runVersion == "v2" {
		data = []byte(`{"error": "injected by error_page service", "version": "v2"}`)
	} else {
		data = []byte(`{"error": "injected by error_page service", "version": "v1"}`)
	}

	_, err := rw.Write(data)
	if err != nil {
		panic(err)
	}
}

func main() {
	ver := "v1"
	if len(os.Args) > 1 {
		ver = os.Args[1]
	}
	ep := &errorPage{
		runVersion: ver,
	}

	err := http.ListenAndServe("0.0.0.0:9011", ep)
	if err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}
