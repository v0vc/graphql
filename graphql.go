package graphql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Client is a client for interacting with a GraphQL API.
type Client struct {
	endpoint                        string
	httpClient                      *http.Client
	useMultipartForm                bool
	defaultWaitAfterTooManyRequests time.Duration

	// closeReq will close the request body immediately allowing for reuse of client
	closeReq bool

	// Log is called with various debug information.
	// To log to standard out, use:
	//  client.Log = func(s string) { log.Println(s) }
	logDebug func(s string)
	logWarn  func(s string)
	logErr   func(s string)
}

// NewClient makes a new Client capable of making GraphQL requests.
func NewClient(endpoint string, opts ...ClientOption) *Client {
	c := &Client{
		endpoint: endpoint,
		logDebug: func(string) {},
		logWarn:  func(string) {},
		logErr:   func(string) {},
	}
	for _, optionFunc := range opts {
		optionFunc(c)
	}
	if c.httpClient == nil {
		c.httpClient = NewRetryableClient(c.logWarn, c.defaultWaitAfterTooManyRequests)
	}
	return c
}

func (c *Client) logDebugf(format string, args ...interface{}) {
	c.logDebug(fmt.Sprintf(format, args...))
}

func (c *Client) logErrorf(format string, args ...interface{}) {
	c.logErr(fmt.Sprintf(format, args...))
}

func (c *Client) logWarnf(format string, args ...interface{}) {
	c.logWarn(fmt.Sprintf(format, args...))
}

// Run executes the query and unmarshals the response from the data field
// into the response object.
// Pass in a nil response object to skip response parsing.
// If the request fails or the server returns an error, the first error
// will be returned.
func (c *Client) Run(ctx context.Context, req *Request, resp interface{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if len(req.files) > 0 && !c.useMultipartForm {
		return errors.New("cannot send files with PostFields option")
	}
	if c.useMultipartForm {
		return c.runWithPostFields(ctx, req, resp)
	}
	return c.runWithJSON(ctx, req, resp)
}

func (c *Client) runWithJSON(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer
	requestBodyObj := struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}{
		Query:     req.q,
		Variables: req.vars,
	}
	if err := json.NewEncoder(&requestBody).Encode(requestBodyObj); err != nil {
		return fmt.Errorf("encode body: %w", err)
	}
	c.logDebugf(">> variables: %v", req.vars)
	c.logDebugf(">> query: %s", req.q)
	gr := &graphResponse{
		Data: resp,
	}
	r, err := http.NewRequest(http.MethodPost, c.endpoint, &requestBody)
	if err != nil {
		return err
	}
	r.Close = c.closeReq
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("Accept", "application/json; charset=utf-8")
	for key, values := range req.Header {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	c.logDebugf(">> headers: %v", r.Header)

	r = r.WithContext(ctx)
	buf, status, err := c.doRequest(r)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		c.logErrorf("server returned a non-200 status code: %v", status)
		c.logErrorf("<< %s", buf.String())
		return fmt.Errorf("graphql: server returned a non-200 status code: %v", status)
	}
	c.logDebugf("<< %s", buf.String())
	if err := json.NewDecoder(&buf).Decode(&gr); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(gr.Errors) > 0 {
		// return first error
		return gr.Errors[0]
	}
	return nil
}

func (c *Client) doRequest(r *http.Request) (bytes.Buffer, int, error) {
	var buf bytes.Buffer
	res, err := c.httpClient.Do(r)
	if err != nil {
		c.logErrorf(">> error: %v", err)
		return buf, http.StatusInternalServerError, err
	}
	defer func(Body io.ReadCloser) {
		er := Body.Close()
		if er != nil {
			fmt.Println(er)
		}
	}(res.Body)
	if _, err := io.Copy(&buf, res.Body); err != nil {
		return buf, res.StatusCode, fmt.Errorf("reading body: %w", err)
	}
	return buf, res.StatusCode, nil
}

func (c *Client) runWithPostFields(ctx context.Context, req *Request, resp interface{}) error {
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	if err := writer.WriteField("query", req.q); err != nil {
		return fmt.Errorf("write query field: %w", err)
	}
	var variablesBuf bytes.Buffer
	if len(req.vars) > 0 {
		variablesField, err := writer.CreateFormField("variables")
		if err != nil {
			return fmt.Errorf("create variables field: %w", err)
		}
		if err := json.NewEncoder(io.MultiWriter(variablesField, &variablesBuf)).Encode(req.vars); err != nil {
			return fmt.Errorf("encode variables: %w", err)
		}
	}
	for i := range req.files {
		part, err := writer.CreateFormFile(req.files[i].Field, req.files[i].Name)
		if err != nil {
			return fmt.Errorf("create form file: %w", err)
		}
		if _, err := io.Copy(part, req.files[i].R); err != nil {
			return fmt.Errorf("preparing file: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close writer: %w", err)
	}
	c.logDebugf(">> variables: %s", variablesBuf.String())
	c.logDebugf(">> files: %d", len(req.files))
	c.logDebugf(">> query: %s", req.q)
	gr := &graphResponse{
		Data: resp,
	}
	r, err := http.NewRequest(http.MethodPost, c.endpoint, &requestBody)
	if err != nil {
		return err
	}
	r.Close = c.closeReq
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("Accept", "application/json; charset=utf-8")
	for key, values := range req.Header {
		for _, value := range values {
			r.Header.Add(key, value)
		}
	}
	c.logDebugf(">> headers: %v", r.Header)
	r = r.WithContext(ctx)
	res, err := c.httpClient.Do(r)
	if err != nil {
		return err
	}
	defer func(Body io.ReadCloser) {
		er := Body.Close()
		if er != nil {
			fmt.Println(er)
		}
	}(res.Body)
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, res.Body); err != nil {
		return fmt.Errorf("reading body: %w", err)
	}
	c.logDebugf("<< %s", buf.String())
	if err := json.NewDecoder(&buf).Decode(&gr); err != nil {
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("graphql: server returned a non-200 status code: %v", res.StatusCode)
		}
		return fmt.Errorf("decoding response: %w", err)
	}
	if len(gr.Errors) > 0 {
		// return first error
		return gr.Errors[0]
	}
	return nil
}

// WithHTTPClient specifies the underlying http.Client to use when
// making requests.
//
//	NewClient(endpoint, WithHTTPClient(specificHTTPClient))
func WithHTTPClient(httpclient *http.Client) ClientOption {
	return func(client *Client) {
		client.httpClient = httpclient
	}
}

// UseMultipartForm uses multipart/form-data and activates support for
// files.
func UseMultipartForm() ClientOption {
	return func(client *Client) {
		client.useMultipartForm = true
	}
}

// ImmediatelyCloseReqBody will close the req body immediately after each request body is ready
func ImmediatelyCloseReqBody() ClientOption {
	return func(client *Client) {
		client.closeReq = true
	}
}

func WithWaitAfterTooManyRequests(duration time.Duration) ClientOption {
	return func(client *Client) {
		client.defaultWaitAfterTooManyRequests = duration
	}
}

func WithLogDebug(logger func(s string)) ClientOption {
	return func(client *Client) {
		client.logDebug = logger
	}
}

func WithLogError(logger func(s string)) ClientOption {
	return func(client *Client) {
		client.logErr = logger
	}
}

func WithLogWarn(logger func(s string)) ClientOption {
	return func(client *Client) {
		client.logWarn = logger
	}
}

// ClientOption are functions that are passed into NewClient to
// modify the behaviour of the Client.
type ClientOption func(*Client)

type graphErr struct {
	Message string
}

func (e graphErr) Error() string {
	return "graphql: " + e.Message
}

type graphResponse struct {
	Data   interface{}
	Errors []graphErr
}

// Request is a GraphQL request.
type Request struct {
	q     string
	vars  map[string]interface{}
	files []File

	// Header represent any request headers that will be set
	// when the request is made.
	Header http.Header
}

// NewRequest makes a new Request with the specified string.
func NewRequest(q string) *Request {
	req := &Request{
		q:      q,
		Header: make(map[string][]string),
	}
	return req
}

// Var sets a variable.
func (req *Request) Var(key string, value interface{}) {
	if req.vars == nil {
		req.vars = make(map[string]interface{})
	}
	req.vars[key] = value
}

// Vars gets the variables for this Request.
func (req *Request) Vars() map[string]interface{} {
	return req.vars
}

// Files gets the files in this request.
func (req *Request) Files() []File {
	return req.files
}

// Query gets the query string of this request.
func (req *Request) Query() string {
	return req.q
}

// File sets a file to upload.
// Files are only supported with a Client that was created with
// the UseMultipartForm option.
func (req *Request) File(fieldName, filename string, r io.Reader) {
	req.files = append(req.files, File{
		Field: fieldName,
		Name:  filename,
		R:     r,
	})
}

// File represents a file to upload.
type File struct {
	Field string
	Name  string
	R     io.Reader
}
