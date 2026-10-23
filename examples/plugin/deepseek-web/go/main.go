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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/deepseek-web/go/internal/dsweb"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName    = "deepseek-web"
	pluginVersion = "0.1.0"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRegistrar        bool                         `json:"model_registrar"`
	ModelProvider         bool                         `json:"model_provider"`
	AuthProvider          bool                         `json:"auth_provider"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type rpcExecutorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

type identifierResponse struct {
	Identifier string `json:"identifier"`
}

type countTokensResponse struct {
	TotalTokens int `json:"total_tokens"`
}

var executor = &dsweb.Executor{
	Client: http.DefaultClient,
	Emit:   emitPluginStreamChunk,
	Close:  closePluginStream,
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
		writeResponse(response, errorEnvelope("invalid_method", "method is required", 0))
		return 1
	}

	var payload []byte
	if request != nil && requestLen > 0 {
		payload = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}

	raw, errHandle := handleMethod(C.GoString(method), payload)
	if errHandle != nil {
		status := 0
		var sc interface{ StatusCode() int }
		if errors.As(errHandle, &sc) && sc != nil {
			status = sc.StatusCode()
		}
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error(), status))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

func handleMethod(method string, payload []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return marshalOK(pluginRegistration())
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return marshalOK(struct{}{})
	case pluginabi.MethodAuthIdentifier:
		return marshalOK(identifierResponse{Identifier: dsweb.Provider})
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("decode auth.parse: %w", err)
		}
		resp, err := dsweb.ParseAuth(req)
		if err != nil {
			return nil, err
		}
		return marshalOK(resp)
	case pluginabi.MethodAuthLoginStart:
		var req pluginapi.AuthLoginStartRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("decode auth.login.start: %w", err)
		}
		resp, err := dsweb.StartLogin(req)
		if err != nil {
			return nil, err
		}
		return marshalOK(resp)
	case pluginabi.MethodAuthLoginPoll:
		var req pluginapi.AuthLoginPollRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("decode auth.login.poll: %w", err)
		}
		resp, err := dsweb.PollLogin(req)
		if err != nil {
			return nil, err
		}
		return marshalOK(resp)
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, fmt.Errorf("decode auth.refresh: %w", err)
		}
		resp, err := dsweb.RefreshAuth(context.Background(), req)
		if err != nil {
			return nil, err
		}
		return marshalOK(resp)
	case pluginabi.MethodModelRegister:
		return marshalOK(pluginapi.ModelRegistrationResponse{
			Provider: dsweb.Provider,
			Models:   dsweb.Models(),
		})
	case pluginabi.MethodModelStatic, pluginabi.MethodModelForAuth:
		return marshalOK(pluginapi.ModelResponse{
			Provider: dsweb.Provider,
			Models:   dsweb.Models(),
		})
	case pluginabi.MethodExecutorIdentifier:
		return marshalOK(identifierResponse{Identifier: dsweb.Provider})
	case pluginabi.MethodExecutorExecute:
		req, err := decodeExecutorRequest(payload)
		if err != nil {
			return nil, err
		}
		resp, err := executor.Execute(context.Background(), req.ExecutorRequest)
		if err != nil {
			return nil, err
		}
		return marshalOK(resp)
	case pluginabi.MethodExecutorExecuteStream:
		req, err := decodeExecutorRequest(payload)
		if err != nil {
			return nil, err
		}
		resp, err := executor.StartStream(context.Background(), req.ExecutorRequest, strings.TrimSpace(req.StreamID))
		if err != nil {
			return nil, err
		}
		return marshalOK(rpcExecutorStreamResponse{Headers: resp.Headers})
	case pluginabi.MethodExecutorCountTokens:
		req, err := decodeExecutorRequest(payload)
		if err != nil {
			return nil, err
		}
		body := req.Payload
		if len(body) == 0 {
			body = req.OriginalRequest
		}
		return marshalOK(pluginapi.ExecutorResponse{
			Payload: mustMarshal(countTokensResponse{TotalTokens: estimateTokens(body)}),
			Headers: http.Header{"Content-Type": []string{"application/json"}},
		})
	case pluginabi.MethodExecutorHTTPRequest:
		return nil, fmt.Errorf("deepseek-web does not expose executor.http_request")
	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

func decodeExecutorRequest(payload []byte) (rpcExecutorRequest, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return rpcExecutorRequest{}, fmt.Errorf("decode executor request: %w", err)
	}
	return req, nil
}

func estimateTokens(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	return (len(body) + 3) / 4
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "vanbienperu3107",
			GitHubRepository: "https://github.com/vanbienperu3107/CLIProxyAPI",
			ConfigFields:     []pluginapi.ConfigField{},
		},
		Capabilities: registrationCapability{
			ModelRegistrar:        true,
			ModelProvider:         true,
			AuthProvider:          true,
			Executor:              true,
			ExecutorModelScope:    pluginapi.ExecutorModelScopeBoth,
			ExecutorInputFormats:  []string{"chat-completions"},
			ExecutorOutputFormats: []string{"chat-completions"},
		},
	}
}

func marshalOK(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func mustMarshal(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func errorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(envelope{
		OK: false,
		Error: &envelopeError{
			Code:       code,
			Message:    message,
			HTTPStatus: httpStatus,
		},
	})
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

func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback %s", method)
		}
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}

	callCode := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)
	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response, code=%d", method, int(callCode))
	}

	var env envelope
	if errUnmarshal := json.Unmarshal(rawResponse, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if callCode != 0 {
		return nil, fmt.Errorf("host callback %s returned code=%d", method, int(callCode))
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errCall
}

func closePluginStream(streamID, errMsg string) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
}

var _ = time.Second
