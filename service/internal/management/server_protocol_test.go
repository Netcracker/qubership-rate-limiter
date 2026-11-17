package management

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	fiberlog "github.com/gofiber/fiber/v3/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The router answers HEAD on every GET route, with the GET's status and no body.
func TestApp_answersHeadOnAGetRoute(t *testing.T) {
	h := newTestAPI(t)
	request := httptest.NewRequest(http.MethodHead, BasePath+"/domains", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Bearer "+testToken(listedCaller))

	response, err := h.app.Test(request, fiber.TestConfig{Timeout: 0})
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()

	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode, "status")
	assert.Empty(t, body, "body")
}

// A request fasthttp refuses while reading it is answered in the TMF envelope
// with a request id, and the refusal leaves no error from the middleware chain
// in the framework log: the catch-all that answers unknown paths must not
// report such a request as one.
func TestApp_answersProtocolErrorsWithoutAMiddlewareFailure(t *testing.T) {
	h := newTestAPI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = h.app.Listener(listener) }()
	t.Cleanup(func() { _ = h.app.Shutdown() })
	frameworkLog := captureFrameworkLog(t)
	path := BasePath + "/domains/" + testDomain + "/counter-resets"
	authorization := "Authorization: Bearer " + testToken(listedCaller) + "\r\n"

	tests := []struct {
		name       string
		request    string
		wantStatus int
	}{
		{
			// The headers alone: fasthttp refuses on the declared length.
			name: "body over the limit",
			request: "POST " + path + " HTTP/1.1\r\nHost: management\r\nContent-Type: application/json\r\n" + authorization +
				"Content-Length: " + strconv.Itoa(maxRequestBody+1) + "\r\n\r\n",
			wantStatus: http.StatusBadRequest,
		},
		{
			// Exactly the read buffer and no end of the headers, so the server
			// reads all of it before it answers and the client sees no reset.
			name:       "headers over the read buffer",
			request:    paddedHeaders("GET "+BasePath+"/domains HTTP/1.1\r\nHost: management\r\n"+authorization, 10240),
			wantStatus: http.StatusRequestHeaderFieldsTooLarge,
		},
		{
			name:       "malformed Content-Length",
			request:    "POST " + path + " HTTP/1.1\r\nHost: management\r\n" + authorization + "Content-Length: abc\r\n\r\n",
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logged := len(frameworkLog.String())

			status, header, raw := sendRawRequest(t, listener.Addr().String(), tt.request)

			require.Equal(t, tt.wantStatus, status, "status, body: %s", raw)
			assert.Equal(t, "application/json", header.Get("Content-Type"), "Content-Type")
			var body errorBody
			require.NoError(t, json.Unmarshal(raw, &body), "body: %s", raw)
			assert.Equal(t, "NC.TMFErrorResponse.v1.0", body.Type, "body: %s", raw)
			assert.Equal(t, strconv.Itoa(tt.wantStatus), body.Status, "body: %s", raw)
			assert.NotEmpty(t, header.Get(RequestIDHeader), "the %s header", RequestIDHeader)
			assert.NotContains(t, frameworkLog.String()[logged:], "middleware traversal failed", "framework log")
		})
	}
}

// paddedHeaders pads head with one header to exactly size bytes, without the
// blank line that ends the headers.
func paddedHeaders(head string, size int) string {
	const name = "X-Padding: "
	return head + name + strings.Repeat("a", size-len(head)-len(name))
}

func sendRawRequest(t *testing.T, addr, request string) (int, http.Header, []byte) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = conn.Write([]byte(request))
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, response.Header, raw
}

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// captureFrameworkLog sends the framework's own log to a buffer until the test
// ends.
func captureFrameworkLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	fiberlog.SetOutput(buffer)
	t.Cleanup(func() { fiberlog.SetOutput(os.Stderr) })
	return buffer
}
