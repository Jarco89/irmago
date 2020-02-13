package irma

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-errors/errors"
	"github.com/hashicorp/go-retryablehttp"
	"github.com/sirupsen/logrus"

	"github.com/privacybydesign/irmago/internal/disable_sigpipe"
	"github.com/privacybydesign/irmago/internal/fs"
)

// HTTPTransport sends and receives JSON messages to a HTTP server.
type HTTPTransport struct {
	Server  string
	client  *retryablehttp.Client
	headers http.Header
}

var HTTPHeaders = map[string]http.Header{}

// Logger is used for logging. If not set, init() will initialize it to logrus.StandardLogger().
var Logger *logrus.Logger

var transportlogger *log.Logger

func init() {
	if Logger == nil {
		Logger = logrus.StandardLogger()
	}
}

// NewHTTPTransport returns a new HTTPTransport.
func NewHTTPTransport(serverURL string) *HTTPTransport {
	if Logger.IsLevelEnabled(logrus.TraceLevel) {
		transportlogger = log.New(Logger.WriterLevel(logrus.TraceLevel), "transport: ", 0)
	} else {
		transportlogger = log.New(ioutil.Discard, "", 0)
	}

	if serverURL != "" && !strings.HasSuffix(serverURL, "/") {
		serverURL += "/"
	}

	// Create a transport that dials with a SIGPIPE handler (which is only active on iOS)
	var innerTransport http.Transport

	innerTransport.Dial = func(network, addr string) (c net.Conn, err error) {
		c, err = net.Dial(network, addr)
		if err != nil {
			return c, err
		}
		if err = disable_sigpipe.DisableSigPipe(c); err != nil {
			return c, err
		}
		return c, nil
	}

	client := &retryablehttp.Client{
		Logger:       transportlogger,
		RetryWaitMin: 100 * time.Millisecond,
		RetryWaitMax: 200 * time.Millisecond,
		RetryMax:     2,
		Backoff:      retryablehttp.DefaultBackoff,
		CheckRetry: func(ctx context.Context, resp *http.Response, err error) (bool, error) {
			// Don't retry on 5xx (which retryablehttp does by default)
			return err != nil || resp.StatusCode == 0, err
		},
		HTTPClient: &http.Client{
			Timeout:   time.Second * 3,
			Transport: &innerTransport,
		},
	}

	var host string
	u, err := url.Parse(serverURL)
	if err != nil {
		Logger.Warnf("failed to parse URL %s: %s", serverURL, err.Error())
	} else {
		host = u.Host
	}
	headers := HTTPHeaders[host].Clone()
	if headers == nil {
		headers = http.Header{}
	}
	return &HTTPTransport{
		Server:  serverURL,
		headers: headers,
		client:  client,
	}
}

// SetHeader sets a header to be sent in requests.
func (transport *HTTPTransport) SetHeader(name, val string) {
	transport.headers.Set(name, val)
}

func (transport *HTTPTransport) request(
	url string, method string, reader io.Reader, isstr bool,
) (response *http.Response, err error) {
	var req retryablehttp.Request
	req.Request, err = http.NewRequest(method, transport.Server+url, reader)
	if err != nil {
		return nil, &SessionError{ErrorType: ErrorTransport, Err: err}
	}
	req.Header = transport.headers.Clone()
	req.Header.Set("User-Agent", "irmago")
	if reader != nil {
		if isstr {
			req.Header.Set("Content-Type", "text/plain; charset=UTF-8")
		} else {
			req.Header.Set("Content-Type", "application/json; charset=UTF-8")
		}
	}
	res, err := transport.client.Do(&req)
	if err != nil {
		return nil, &SessionError{ErrorType: ErrorTransport, Err: err}
	}
	return res, nil
}

func (transport *HTTPTransport) jsonRequest(url string, method string, result interface{}, object interface{}) error {
	if method != http.MethodPost && method != http.MethodGet && method != http.MethodDelete {
		panic("Unsupported HTTP method " + method)
	}
	if method == http.MethodGet && object != nil {
		panic("Cannot GET and also post an object")
	}

	var isstr bool
	var reader io.Reader
	if object != nil {
		var objstr string
		if objstr, isstr = object.(string); isstr {
			Logger.Trace("transport: body: ", objstr)
			reader = bytes.NewBuffer([]byte(objstr))
		} else {
			marshaled, err := json.Marshal(object)
			if err != nil {
				return &SessionError{ErrorType: ErrorSerialization, Err: err}
			}
			Logger.Trace("transport: body: ", string(marshaled))
			reader = bytes.NewBuffer(marshaled)
		}
	}

	res, err := transport.request(url, method, reader, isstr)
	if err != nil {
		return err
	}
	if method == http.MethodDelete {
		return nil
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return &SessionError{ErrorType: ErrorServerResponse, Err: err, RemoteStatus: res.StatusCode}
	}
	if res.StatusCode != 200 {
		apierr := &RemoteError{}
		err = json.Unmarshal(body, apierr)
		if err != nil || apierr.ErrorName == "" { // Not an ApiErrorMessage
			return &SessionError{ErrorType: ErrorServerResponse, RemoteStatus: res.StatusCode}
		}
		Logger.Tracef("transport: error: %+v", apierr)
		return &SessionError{ErrorType: ErrorApi, RemoteStatus: res.StatusCode, RemoteError: apierr}
	}

	Logger.Tracef("transport: response: %s", string(body))
	if _, resultstr := result.(*string); resultstr {
		*result.(*string) = string(body)
	} else {
		err = UnmarshalValidate(body, result)
		if err != nil {
			return &SessionError{ErrorType: ErrorServerResponse, Err: err, RemoteStatus: res.StatusCode}
		}
	}

	return nil
}

func (transport *HTTPTransport) GetBytes(url string) ([]byte, error) {
	res, err := transport.request(url, http.MethodGet, nil, false)
	if err != nil {
		return nil, &SessionError{ErrorType: ErrorTransport, Err: err}
	}

	if res.StatusCode != 200 {
		return nil, &SessionError{ErrorType: ErrorServerResponse, RemoteStatus: res.StatusCode}
	}
	b, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, &SessionError{ErrorType: ErrorServerResponse, Err: err, RemoteStatus: res.StatusCode}
	}
	return b, nil
}

func (transport *HTTPTransport) GetSignedFile(url string, dest string, hash ConfigurationFileHash) error {
	b, err := transport.GetBytes(url)
	if err != nil {
		return err
	}
	sha := sha256.Sum256(b)
	if hash != nil && !bytes.Equal(hash, sha[:]) {
		return errors.Errorf("Signature over new file %s is not valid", dest)
	}
	if err = fs.EnsureDirectoryExists(filepath.Dir(dest)); err != nil {
		return err
	}
	return fs.SaveFile(dest, b)
}

func (transport *HTTPTransport) GetFile(url string, dest string) error {
	return transport.GetSignedFile(url, dest, nil)
}

// Post sends the object to the server and parses its response into result.
func (transport *HTTPTransport) Post(url string, result interface{}, object interface{}) error {
	return transport.jsonRequest(url, http.MethodPost, result, object)
}

// Get performs a GET request and parses the server's response into result.
func (transport *HTTPTransport) Get(url string, result interface{}) error {
	return transport.jsonRequest(url, http.MethodGet, result, nil)
}

// Delete performs a DELETE.
func (transport *HTTPTransport) Delete() {
	_ = transport.jsonRequest("", http.MethodDelete, nil, nil)
}
