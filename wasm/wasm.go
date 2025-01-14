package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/wasmerio/wasmer-go/wasmer"
)

type LogLevel int32

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
	LogLevelCritical
)

type Status int32

const (
	StatusOK Status = iota
	StatusInternalFailure
	StatusInvalidArgument
	StatusNotFound
)

func main() {
	wasmBytes, err := ioutil.ReadFile("sample-wasm/target/wasm32-unknown-unknown/debug/examples/decode_msgpack.wasm")
	if err != nil {
		panic(err)
	}
	log.Printf("WASM size: %v", humanize.Bytes(uint64(len(wasmBytes))))

	wm, err := newWasmModule(wasmBytes)
	if err != nil {
		log.Fatal("Failed to create module:", err)
	}

	rtn, err := wm.process()
	if err != nil {
		log.Fatal("Failed to execute process().", err)
	}
	log.Println("Done. Return code: ", rtn)
}

type wasmModule struct {
	instance *wasmer.Instance

	mallocFunc  wasmer.NativeFunction
	processFunc wasmer.NativeFunction
}

func newWasmModule(wasmData []byte) (*wasmModule, error) {
	// Create an Engine
	engine := wasmer.NewEngine()

	// Create a Store
	store := wasmer.NewStore(engine)

	log.Println("Compiling module...")
	module, err := wasmer.NewModule(store, wasmData)
	if err != nil {
		return nil, fmt.Errorf("failed to compile module: %w", err)
	}

	wm := &wasmModule{}

	importObject := wasmer.NewImportObject()
	importObject.Register(
		"elastic",
		map[string]wasmer.IntoExtern{
			"elastic_get_field": wasmer.NewFunction(
				store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32)),
				wm.getField,
			),
			"elastic_put_field": wasmer.NewFunction(
				store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32)),
				wm.putField,
			),
			"elastic_log": wasmer.NewFunction(
				store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				wm.log,
			),
			"elastic_get_current_time_nanoseconds": wasmer.NewFunction(
				store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				wm.getCurrentTime,
			),
		},
	)

	wm.instance, err = wasmer.NewInstance(module, importObject)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate the module: %w", err)
	}

	wm.mallocFunc, err = wm.instance.Exports.GetFunction("malloc")
	if err != nil {
		return nil, fmt.Errorf("failed to find malloc export: %w", err)
	}

	wm.processFunc, err = wm.instance.Exports.GetFunction("process")
	if err != nil {
		return nil, fmt.Errorf("failed to find process export: %w", err)
	}

	return wm, nil
}

func (m *wasmModule) getField(args []wasmer.Value) ([]wasmer.Value, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("get_field requires 4 arguments, but got %d", len(args))
	}

	dataPtr := args[0].I32()
	dataLen := args[1].I32()
	rtnPtr := args[2].I32()
	rtnLen := args[3].I32()

	memory, err := m.instance.Exports.GetMemory("memory")
	if err != nil {
		return nil, fmt.Errorf("failed to get the `memory` memory: %w", err)
	}

	data := memory.Data()[dataPtr : dataPtr+dataLen]
	log.Println("get_field: ", string(data))

	if string(data) == "message" {
		raw := "df00000001a464617461ab68656c6c6f20776f726c64"
		value, err := json.Marshal(raw)
		if err != nil {
			return nil, err
		}

		valueSize := int32(len(value))

		valuePtr, err := m.malloc(valueSize)
		if err != nil {
			return nil, err
		}

		// Copy into allocated memory.
		copy(memory.Data()[valuePtr:valuePtr+valueSize], value)

		binary.LittleEndian.PutUint32(memory.Data()[rtnPtr:rtnPtr+4], uint32(valuePtr))
		binary.LittleEndian.PutUint32(memory.Data()[rtnLen:rtnLen+4], uint32(valueSize))

		return []wasmer.Value{wasmer.NewI32(0)}, nil
	}

	return []wasmer.Value{wasmer.NewI32(0)}, nil
}

func (m *wasmModule) putField(args []wasmer.Value) ([]wasmer.Value, error) {
	if len(args) != 4 {
		return nil, fmt.Errorf("put_field requires 4 arguments, but got %d", len(args))
	}

	keyPtr := args[0].I32()
	keyLen := args[1].I32()
	valuePtr := args[2].I32()
	valueLen := args[3].I32()

	memory, err := m.instance.Exports.GetMemory("memory")
	if err != nil {
		return []wasmer.Value{wasmer.NewI32(int32(StatusInternalFailure))}, fmt.Errorf("failed to get the `memory` memory: %w", err)
	}

	key := memory.Data()[keyPtr : keyPtr+keyLen]
	value := memory.Data()[valuePtr : valuePtr+valueLen]
	log.Println("put_field: ", string(key), string(value))

	var v interface{}
	if err := json.Unmarshal(value, &v); err != nil {
		return []wasmer.Value{wasmer.NewI32(int32(StatusInvalidArgument))}, fmt.Errorf("failed to decode value: %w", err)
	}

	log.Printf("put_field: %s=%+v", key, v)

	return []wasmer.Value{wasmer.NewI32(int32(StatusOK))}, nil
}

func (m *wasmModule) log(args []wasmer.Value) ([]wasmer.Value, error) {
	if len(args) != 3 {
		return nil, fmt.Errorf("log requires 3 arguments, but got %d", len(args))
	}

	level := args[0].I32()
	dataPtr := args[1].I32()
	dataLen := args[2].I32()

	memory, err := m.instance.Exports.GetMemory("memory")
	if err != nil {
		return nil, fmt.Errorf("failed to get the `memory` memory: %w", err)
	}

	data := memory.Data()[dataPtr : dataPtr+dataLen]
	log.Printf("log[%d]: %s", level, string(data))
	return []wasmer.Value{wasmer.NewI32(0)}, nil
}

func (m *wasmModule) getCurrentTime(args []wasmer.Value) ([]wasmer.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("elastic_get_current_time_nanoseconds requires 1 arguments, but got %d", len(args))
	}

	ptr := args[0].I32()

	memory, err := m.instance.Exports.GetMemory("memory")
	if err != nil {
		return nil, fmt.Errorf("failed to get the `memory` memory: %w", err)
	}

	binary.LittleEndian.PutUint64(memory.Data()[ptr:ptr+8], uint64(time.Now().UnixNano()))
	return []wasmer.Value{wasmer.NewI32(0)}, nil
}

func (m *wasmModule) malloc(size int32) (wasmPointer int32, err error) {
	ptr, err := m.mallocFunc(size)
	if err != nil {
		return 0, err
	}
	return ptr.(int32), nil
}

func (m *wasmModule) process() (int32, error) {
	rtn, err := m.processFunc()
	if err != nil {
		return 0, err
	}
	return rtn.(int32), nil
}
