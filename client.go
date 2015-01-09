package rollbar

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"sync"
	"time"
)

type Client interface {
	// Rollbar access token.
	SetToken(token string)
	// All errors and messages will be submitted under this environment.
	SetEnvironment(environment string)
	// String describing the running code version on the server
	SetCodeVersion(codeVersion string)
	// host: The server hostname. Will be indexed.
	SetServerHost(serverHost string)
	// root: Path to the application code root, not including the final slash.
	// Used to collapse non-project code when displaying tracebacks.
	SetServerRoot(serverRoot string)

	Error(level string, err error)
	ErrorWithExtras(level string, err error, extras map[string]interface{})
	ErrorWithStackSkip(level string, err error, skip int)
	ErrorWithStackSkipWithExtras(level string, err error, skip int, extras map[string]interface{})

	RequestError(level string, r *http.Request, err error)
	RequestErrorWithExtras(level string, r *http.Request, err error, extras map[string]interface{})
	RequestErrorWithStackSkip(level string, r *http.Request, err error, skip int)
	RequestErrorWithStackSkipWithExtras(level string, r *http.Request, err error, skip int, extras map[string]interface{})

	Message(level string, msg string)
	MessageWithExtras(level string, msg string, extras map[string]interface{})

	Wait()
}

// Rollbar is the default concrete implementation of the Client interface
type Rollbar struct {
	// Rollbar access token. If this is blank, no errors will be reported to
	// Rollbar.
	Token string
	// All errors and messages will be submitted under this environment.
	Environment string
	// API endpoint for Rollbar.
	Endpoint string
	// Maximum number of errors allowed in the sending queue before we start
	// dropping new errors on the floor.
	Buffer int
	// Filter HTTP Headers parameters from being sent to Rollbar.
	FilterHeaders *regexp.Regexp
	// Filter GET and POST parameters from being sent to Rollbar.
	FilterFields *regexp.Regexp
	// String describing the running code version on the server
	CodeVersion string
	// host: The server hostname. Will be indexed.
	ServerHost string
	// root: Path to the application code root, not including the final slash.
	// Used to collapse non-project code when displaying tracebacks.
	ServerRoot string
	// Queue of messages to be sent.
	bodyChannel chan map[string]interface{}
	waitGroup   sync.WaitGroup
}

// New returns the default implementation of a Client
func New(token, environment, codeVersion, serverHost, serverRoot string) Client {
	buffer := 1000
	client := &Rollbar{
		Token:         token,
		Environment:   environment,
		Endpoint:      "https://api.rollbar.com/api/1/item/",
		Buffer:        1000,
		FilterHeaders: regexp.MustCompile("Authorization"),
		FilterFields:  regexp.MustCompile("password|secret|token"),
		CodeVersion:   codeVersion,
		ServerHost:    serverHost,
		ServerRoot:    serverRoot,
		bodyChannel:   make(chan map[string]interface{}, buffer),
	}

	go func() {
		for body := range client.bodyChannel {
			client.post(body)
			client.waitGroup.Done()
		}
	}()
	return client
}

func (c *Rollbar) SetToken(token string) {
	c.Token = token
}

func (c *Rollbar) SetEnvironment(environment string) {
	c.Environment = environment
}

func (c *Rollbar) SetCodeVersion(codeVersion string) {
	c.CodeVersion = codeVersion
}

func (c *Rollbar) SetServerHost(serverHost string) {
	c.ServerHost = serverHost
}

func (c *Rollbar) SetServerRoot(serverRoot string) {
	c.ServerRoot = serverRoot
}

// -- Error reporting

var noExtras map[string]interface{}

// Error asynchronously sends an error to Rollbar with the given severity level.
func (c *Rollbar) Error(level string, err error) {
	c.ErrorWithExtras(level, err, noExtras)
}

// ErrorWithExtras asynchronously sends an error to Rollbar with the given severity level with extra custom data.
func (c *Rollbar) ErrorWithExtras(level string, err error, extras map[string]interface{}) {
	c.ErrorWithStackSkipWithExtras(level, err, 1, extras)
}

// RequestError asynchronously sends an error to Rollbar with the given
// severity level and request-specific information.
func (c *Rollbar) RequestError(level string, r *http.Request, err error) {
	c.RequestErrorWithExtras(level, r, err, noExtras)
}

// RequestErrorWithExtras asynchronously sends an error to Rollbar with the given
// severity level and request-specific information with extra custom data.
func (c *Rollbar) RequestErrorWithExtras(level string, r *http.Request, err error, extras map[string]interface{}) {
	c.RequestErrorWithStackSkipWithExtras(level, r, err, 1, extras)
}

// ErrorWithStackSkip asynchronously sends an error to Rollbar with the given
// severity level and a given number of stack trace frames skipped.
func (c *Rollbar) ErrorWithStackSkip(level string, err error, skip int) {
	c.ErrorWithStackSkipWithExtras(level, err, skip, noExtras)
}

// ErrorWithStackSkipWithExtras asynchronously sends an error to Rollbar with the given
// severity level and a given number of stack trace frames skipped with extra custom data.
func (c *Rollbar) ErrorWithStackSkipWithExtras(level string, err error, skip int, extras map[string]interface{}) {
	body := c.buildBody(level, err.Error(), extras)
	data := body["data"].(map[string]interface{})
	errBody, fingerprint := errorBody(err, skip)
	data["body"] = errBody
	data["fingerprint"] = fingerprint

	c.push(body)
}

// RequestErrorWithStackSkip asynchronously sends an error to Rollbar with the
// given severity level and a given number of stack trace frames skipped, in
// addition to extra request-specific information.
func (c *Rollbar) RequestErrorWithStackSkip(level string, r *http.Request, err error, skip int) {
	c.RequestErrorWithStackSkipWithExtras(level, r, err, skip, noExtras)
}

// RequestErrorWithStackSkip asynchronously sends an error to Rollbar with the
// given severity level and a given number of stack trace frames skipped, in
// addition to extra request-specific information and extra custom data.
func (c *Rollbar) RequestErrorWithStackSkipWithExtras(level string, r *http.Request, err error, skip int, extras map[string]interface{}) {
	body := c.buildBody(level, err.Error(), extras)
	data := body["data"].(map[string]interface{})

	errBody, fingerprint := errorBody(err, skip)
	data["body"] = errBody
	data["fingerprint"] = fingerprint

	data["request"] = c.errorRequest(r)

	c.push(body)
}

// -- Message reporting

// Message asynchronously sends a message to Rollbar with the given severity
// level. Rollbar request is asynchronous.
func (c *Rollbar) Message(level string, msg string) {
	c.MessageWithExtras(level, msg, noExtras)
}

// Message asynchronously sends a message to Rollbar with the given severity
// level with extra custom data. Rollbar request is asynchronous.
func (c *Rollbar) MessageWithExtras(level string, msg string, extras map[string]interface{}) {
	body := c.buildBody(level, msg, extras)
	data := body["data"].(map[string]interface{})
	data["body"] = messageBody(msg)

	c.push(body)
}

// -- Misc.

// Wait will block until the queue of errors / messages is empty.
func (c *Rollbar) Wait() {
	c.waitGroup.Wait()
}

// Build the main JSON structure that will be sent to Rollbar with the
// appropriate metadata.
func (c *Rollbar) buildBody(level, title string, extras map[string]interface{}) map[string]interface{} {
	timestamp := time.Now().Unix()
	data := map[string]interface{}{
		"environment":  c.Environment,
		"title":        title,
		"level":        level,
		"timestamp":    timestamp,
		"platform":     runtime.GOOS,
		"language":     "go",
		"code_version": c.CodeVersion,
		"server": map[string]interface{}{
			"host": c.ServerHost,
			"root": c.ServerRoot,
		},
		"notifier": map[string]interface{}{
			"name":    NAME,
			"version": VERSION,
		},
	}

	for k, v := range extras {
		data[k] = v
	}

	return map[string]interface{}{
		"access_token": c.Token,
		"data":         data,
	}
}

// Extract error details from a Request to a format that Rollbar accepts.
func (c *Rollbar) errorRequest(r *http.Request) map[string]interface{} {
	cleanQuery := filterParams(c.FilterFields, r.URL.Query())

	return map[string]interface{}{
		"url":     r.URL.String(),
		"method":  r.Method,
		"headers": flattenValues(filterParams(c.FilterHeaders, r.Header)),

		// GET params
		"query_string": url.Values(cleanQuery).Encode(),
		"GET":          flattenValues(cleanQuery),

		// POST / PUT params
		"POST": flattenValues(filterParams(c.FilterFields, r.Form)),
	}
}

// filterParams filters sensitive information like passwords from being sent to
// Rollbar.
func filterParams(pattern *regexp.Regexp, values map[string][]string) map[string][]string {
	for key, _ := range values {
		if pattern.Match([]byte(key)) {
			values[key] = []string{FILTERED}
		}
	}

	return values
}

func flattenValues(values map[string][]string) map[string]interface{} {
	result := make(map[string]interface{})

	for k, v := range values {
		if len(v) == 1 {
			result[k] = v[0]
		} else {
			result[k] = v
		}
	}

	return result
}

// -- POST handling

// Queue the given JSON body to be POSTed to Rollbar.
func (c *Rollbar) push(body map[string]interface{}) {
	if len(c.bodyChannel) < c.Buffer {
		c.waitGroup.Add(1)
		c.bodyChannel <- body
	} else {
		rollbarError("buffer full, dropping error on the floor")
	}
}

// POST the given JSON body to Rollbar synchronously.
func (c *Rollbar) post(body map[string]interface{}) {
	if len(c.Token) == 0 {
		rollbarError("empty token")
		return
	}

	jsonBody, err := json.Marshal(body)
	if err != nil {
		rollbarError("failed to encode payload: %s", err.Error())
		return
	}

	resp, err := http.Post(c.Endpoint, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		rollbarError("POST failed: %s", err.Error())
	} else if resp.StatusCode != 200 {
		rollbarError("received response: %s", resp.Status)
	}
	if resp != nil {
		resp.Body.Close()
	}
}
