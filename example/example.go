package main

import (
	"embed"
	_ "embed" // for go:embed
	"io/fs"
	"log"
	"net/http"
)

//go:embed public
var public embed.FS

func main() {
	fsys, _ := fs.Sub(public, "public")
	http.Handle("/", http.FileServer(http.FS(fsys)))

	log.Println("listening on http://localhost:8080")

	err := http.ListenAndServe(":8080", nil)
	log.Fatalln(err)
}
