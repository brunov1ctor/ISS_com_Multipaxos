// Biblioteca compartilhada iss_multicast: expõe o clientlib com interface C, para ser usada
// de qualquer linguagem (C/C++ com #include, Python com ctypes, Java com JNA, Rust com FFI...).
//
// Compilar (gera também o iss_multicast.h):
//
//	Windows: go build -buildmode=c-shared -o iss_multicast.dll ./cmd/libissmulticast
//	Linux:   go build -buildmode=c-shared -o libiss_multicast.so ./cmd/libissmulticast
//
// A ordem total (local/global) é configuração do sistema (TotalOrder, definido no deploy),
// não do cliente: misturar modos quebraria a garantia global.
//
// Convenção de retorno: 0 = sucesso, -1 = erro (a mensagem fica em iss_last_error()).
package main

/*
#include <stdlib.h>
*/
import "C"

import (
	"sync"
	"unsafe"

	"github.com/hyperledger-labs/mirbft/clientlib"
)

var (
	mu      sync.Mutex
	client  *clientlib.Client
	lastErr string
)

func fail(err error) C.int {
	mu.Lock()
	lastErr = err.Error()
	mu.Unlock()
	return -1
}

func get() (*clientlib.Client, C.int) {
	mu.Lock()
	defer mu.Unlock()
	if client == nil {
		lastErr = "iss_connect não foi chamado"
		return nil, -1
	}
	return client, 0
}

//export iss_connect
func iss_connect(configFile, discoveryAddr *C.char) C.int {
	c, err := clientlib.Connect(C.GoString(configFile), C.GoString(discoveryAddr))
	if err != nil {
		return fail(err)
	}
	mu.Lock()
	client = c
	mu.Unlock()
	return 0
}

// iss_multicast envia msg para o grupo. A ordem (local ou global) é do sistema, definida no
// deploy (./deploy.sh ... --order global), não pelo cliente.
//
//export iss_multicast
func iss_multicast(group C.uint, msg *C.char) C.int {
	c, rc := get()
	if rc != 0 {
		return rc
	}
	if _, err := c.Multicast(uint32(group), []byte(C.GoString(msg))); err != nil {
		return fail(err)
	}
	return 0
}

//export iss_put
func iss_put(group C.uint, key, value *C.char) C.int {
	c, rc := get()
	if rc != 0 {
		return rc
	}
	if err := c.Put(uint32(group), C.GoString(key), []byte(C.GoString(value))); err != nil {
		return fail(err)
	}
	return 0
}

// iss_get lê key do grupo. Devolve 1 se achou (*out recebe uma string que deve ser liberada
// com iss_free), 0 se a chave não existe, -1 em erro.
//
//export iss_get
func iss_get(group C.uint, key *C.char, out **C.char) C.int {
	c, rc := get()
	if rc != 0 {
		return rc
	}
	v, found, err := c.Get(uint32(group), C.GoString(key))
	if err != nil {
		return fail(err)
	}
	if !found {
		*out = nil
		return 0
	}
	*out = C.CString(string(v))
	return 1
}

//export iss_free
func iss_free(p *C.char) { C.free(unsafe.Pointer(p)) }

// iss_last_error devolve a última mensagem de erro (liberar com iss_free).
//
//export iss_last_error
func iss_last_error() *C.char {
	mu.Lock()
	defer mu.Unlock()
	return C.CString(lastErr)
}

//export iss_close
func iss_close() {
	mu.Lock()
	defer mu.Unlock()
	if client != nil {
		client.Close()
		client = nil
	}
}

func main() {}
