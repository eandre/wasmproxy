# wasmproxy

This is a simple prototype of running a Go server as an in-browser proxy using wasm + service workers.

Unfortunately(?) it doesn't allow for rewriting CORS requests as requests made through wasm still apply `fetch` CORS restrictions.

## Building
```
cd proxy
GOOS=js GOARCH=wasm go build -o ../example/public/server/proxy.wasm
```

## Running

```
cd example
go run example.go
```
