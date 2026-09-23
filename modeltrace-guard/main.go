// Package main implements the modeltrace-guard CLIProxyAPI plugin.
//
// The plugin observes per-request routing through the usage plugin hook,
// probes configured credentials with the ModelTrace long-integer challenge
// suite, scores the digit outputs against the ModelTrace fingerprint bank,
// and exposes results through management routes and a resource dashboard.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	pluginID      = "modeltrace-guard"
	pluginVersion = "0.1.0"
)

var usageCount atomic.Int64

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type registration struct {
	SchemaVersion uint32                   `json:"schema_version"`
	Metadata      pluginapi.Metadata       `json:"metadata"`
	Capabilities  registrationCapabilities `json:"capabilities"`
}

type registrationCapabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	stopProber()
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		applyConfigYAML(request)
		ensureProber()
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		stopProber()
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginShutdown:
		stopProber()
		return okEnvelope(map[string]any{})
	case pluginabi.MethodUsageHandle:
		usageCount.Add(1)
		return handleUsage(request)
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           "cpa-modeltrace-plugin contributors",
			GitHubRepository: "https://github.com/router-for-me/cpa-modeltrace-plugin",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "bank_path", Type: pluginapi.ConfigFieldTypeString, Description: "Path to the ModelTrace unified_bank.json fingerprint bank file."},
				{Name: "interval_minutes", Type: pluginapi.ConfigFieldTypeInteger, Description: "Minutes between automatic probe runs; 0 disables scheduled probing."},
				{Name: "probes_per_run", Type: pluginapi.ConfigFieldTypeInteger, Description: "Number of long-integer challenge probes per run (1-3)."},
				{Name: "providers", Type: pluginapi.ConfigFieldTypeString, Description: "Comma-separated provider filter, for example codex,claude. Empty keeps every credential."},
			},
		},
		Capabilities: registrationCapabilities{
			UsagePlugin:   true,
			ManagementAPI: true,
		},
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// callHost invokes a host callback method and unmarshals the JSON result.
func callHost(method string, payload any, result any) error {
	request := []byte(nil)
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return errMarshal
		}
		request = raw
	}
	var methodC *C.char
	{
		cMethod := C.CString(method)
		defer C.free(unsafe.Pointer(cMethod))
		methodC = cMethod
	}
	var responseBuffer C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(request) > 0 {
		requestPtr = (*C.uint8_t)(unsafe.Pointer(&request[0]))
	}
	code := C.call_host_api(methodC, requestPtr, C.size_t(len(request)), &responseBuffer)
	if code != 0 {
		return errHostCallback{method: method}
	}
	defer func() {
		if responseBuffer.ptr != nil {
			C.free_host_buffer(responseBuffer.ptr, responseBuffer.len)
		}
	}()
	if result == nil {
		return nil
	}
	body := C.GoBytes(responseBuffer.ptr, C.int(responseBuffer.len))
	if errUnmarshal := json.Unmarshal(body, result); errUnmarshal != nil {
		return errHostCallback{method: method, cause: errUnmarshal}
	}
	return nil
}

type errHostCallback struct {
	method string
	cause  error
}

func (e errHostCallback) Error() string {
	if e.cause != nil {
		return "host callback " + e.method + " failed: " + e.cause.Error()
	}
	return "host callback " + e.method + " failed"
}

func (e errHostCallback) Unwrap() error { return e.cause }

// buildManagementRequest unmarshals a management.handle request payload.
func buildManagementRequest(raw []byte) (managementRequest, error) {
	req := managementRequest{}
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return req, errUnmarshal
	}
	return req, nil
}

// managementRequest mirrors the host management.handle payload.
type managementRequest struct {
	Method         string
	Path           string
	Headers        http.Header
	Query          url.Values
	Body           []byte
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// managementResponse mirrors the host management.handle result.
type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}
