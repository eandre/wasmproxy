importScripts('https://cdn.jsdelivr.net/gh/golang/go@go1.18.2/misc/wasm/wasm_exec.js')

const SCOPE = "/server/"

const handlerPromise = new Promise(setHandler => {
  self.wasmhttp = {
    path: SCOPE,
    setHandler,
  }
})

const wasm = "/server/proxy.wasm"
const go = new Go()
go.argv = [wasm]
WebAssembly.instantiateStreaming(fetch(wasm), go.importObject).then(({ instance }) => {
  return go.run(instance)
})

self.addEventListener('fetch', e => {
  const { pathname } = new URL(e.request.url)
  if (!pathname.startsWith(SCOPE)) return

  e.respondWith(handlerPromise.then(handler => handler(e.request)))
})

// Skip installed stage and jump to activating stage
self.addEventListener('install', (event) => {
  event.waitUntil(self.skipWaiting())
})

// Start controlling clients as soon as the SW is activated
self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim())
})