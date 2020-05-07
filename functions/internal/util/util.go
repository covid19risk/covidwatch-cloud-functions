package util

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testFirestoreContextKey is a key used to identify a *TestFirestore object.
// Since it is not exported, there is no way for code outside of this package to
// overwrite or access values stored with this key.
type testFirestoreContextKey struct{}

// WithTestFirestore wraps parent, producing a context.Context which stores the
// given *TestFirestore. If NewContext is called with an *http.Request whose
// context.Context was produced using this function, it will connect to the
// TestFirestore instead of consulting the environment to figure out how to
// connect to a Firestore instance, as is its normal behavior.
func WithTestFirestore(parent context.Context, store *TestFirestore) context.Context {
	return context.WithValue(parent, testFirestoreContextKey{}, store)
}

// fakeClockKey is a key used to identify the fake clock. Since it is not
// exported, there is no way for code outside of this package to overwrite or
// access values stored with this key.
type fakeClockKey struct{}

// WithFakeClock stores a fake time in a context. If Now is called on the
// returned context, the given fake time will be returned instead of the real
// time.
func WithFakeClock(parent context.Context, time time.Duration) context.Context {
	return context.WithValue(parent, fakeClockKey{}, &time)
}

// Now attempts to extract a fake clock from ctx. If a fake clock was attached
// using WithFakeClock, then it is returned. If no fake clock is found, then the
// result of time.Now() is returned.
func Now(ctx context.Context) time.Time {
	switch t := ctx.Value(fakeClockKey{}).(type) {
	case nil:
		return time.Now()
	case *time.Duration:
		return time.Unix(0, int64(*t))
	default:
		panic("unreachable")
	}
}

// Context is a context.Context that provides extra utilities for common
// operations.
type Context struct {
	resp   http.ResponseWriter
	req    *http.Request
	client *firestore.Client

	context.Context
}

// NewContext constructs a new Context from an http.ResponseWriter and an
// *http.Request.
//
// NewContext automatically figures out how to connect to a Firestore instance.
// In particular:
//  - In a production environment, it uses the firestore.DetectProjectID
//    project, which causes firestore.NewClient to use environment variables
//    to detect the appropriate Firestore configuration.
// - If r.Context() was created using WithTestFirestore, NewContext will connect
//   to that TestFirestore instance.
// - If the `FIRESTORE_EMULATOR_HOST` environment variable is set,
//   firestore.NewClient will attempt to connect to that host.
func NewContext(w http.ResponseWriter, r *http.Request) (Context, StatusError) {
	ctx := r.Context()

	// In production, automatically detect credentials from the environment.
	projectID := firestore.DetectProjectID
	// Attempt to extract a *TestFirestore stored by withTestFirestore.
	testFirestore := ctx.Value(testFirestoreContextKey{})
	var clientOptions []option.ClientOption
	if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" || testFirestore != nil {
		// If we're not in production, then `firestore.DetectProjectID` will
		// cause `NewClient` to look for credentials which aren't there, and so
		// the call will fail.
		projectID = "test"
		if store, ok := testFirestore.(*TestFirestore); ok {
			opt, err := store.clientOption()
			if err != nil {
				return Context{}, NewInternalServerError(err)
			}
			clientOptions = append(clientOptions, opt)
			projectID = store.projectID
		}
	}
	client, err := firestore.NewClient(ctx, projectID, clientOptions...)
	if err != nil {
		err := NewInternalServerError(err)
		return Context{}, err
	}

	return Context{resp: w,
		req:     r,
		client:  client,
		Context: ctx,
	}, nil
}

// HTTPRequest returns the *http.Request that was used to construct this
// Context.
func (c *Context) HTTPRequest() *http.Request {
	return c.req
}

// HTTPResponseWriter returns the http.ResponseWriter that was used to construct
// this Context.
func (c *Context) HTTPResponseWriter() http.ResponseWriter {
	return c.resp
}

// FirestoreClient returns the firestore Client.
func (c *Context) FirestoreClient() *firestore.Client {
	return c.client
}

// ValidateRequestMethod validates that c.HTTPRequest().Method == method, and if
// not, returns an appropriate StatusError.
func (c *Context) ValidateRequestMethod(method, err string) StatusError {
	m := c.HTTPRequest().Method
	if m != method {
		return NewMethodNotAllowedError(m)
	}
	return nil
}

// RunTransaction wraps firestore.Client.RunTransaction. If the transaction
// fails for reasons other than f failing, the resulting error will be wrapped
// with NewInternalStatusError.
func (c *Context) RunTransaction(f func(ctx context.Context, txn *firestore.Transaction) StatusError) StatusError {
	return RunTransaction(c, c.FirestoreClient(), f)
}

// RunTransaction wraps firestore.Client.RunTransaction. If the transaction
// fails for reasons other than f failing, the resulting error will be wrapped
// with NewInternalStatusError.
func RunTransaction(ctx context.Context, c *firestore.Client, f func(ctx context.Context, txn *firestore.Transaction) StatusError) StatusError {
	err := c.RunTransaction(
		ctx,
		func(ctx context.Context, txn *firestore.Transaction) error {
			return f(ctx, txn)
		},
	)
	switch err := err.(type) {
	case nil:
		return nil
	case StatusError:
		return err
	default:
		// If err doesn't implement StatusError, then it must not have come from
		// f, which means that it was an error with running the transaction not
		// related to business logic, so it's an internal server error.
		return NewInternalServerError(err)
	}
}

// StatusError is implemented by error types which correspond to a particular
// HTTP status code.
type StatusError interface {
	error

	// HTTPStatusCode returns the HTTP status code for this error.
	HTTPStatusCode() int
	// Message returns a string which will be used as the contents of the
	// "message" field in the JSON object which is sent as the response body.
	Message() string
}

type statusError struct {
	code int
	// If message is non-empty, then Message will return it. Otherwise, Message
	// will return error.Error().
	message string
	error
}

func (e statusError) HTTPStatusCode() int {
	return e.code
}

func (e statusError) Message() string {
	if e.message != "" {
		return e.message
	}
	return e.error.Error()
}

// NewInternalServerError wraps err in a StatusError whose HTTPStatusCode method
// returns http.StatusInternalServerError and whose Message method returns
// "internal server error" to avoid leaking potentially sensitive data from err.
func NewInternalServerError(err error) StatusError {
	return statusError{
		code: http.StatusInternalServerError,
		// We don't want to leak any potentially sensitive data that might be
		// contained in the error. This message will be sent to the client
		// instead of err.Error().
		message: "internal server error",
		error:   err,
	}
}

// NewBadRequestError wraps err in a StatusError whose HTTPStatusCode method
// returns http.StatusBadRequest and whose Message method returns err.Error().
func NewBadRequestError(err error) StatusError {
	return statusError{
		code:  http.StatusBadRequest,
		error: err,
	}
}

// NewMethodNotAllowedError wraps err in a StatusError whose HTTPStatusCode
// method returns http.StatusMethodNotAllowed and whose Message method returns
// "unsupported method: " followed by the given method string.

func NewMethodNotAllowedError(method string) StatusError {
	return statusError{
		code:  http.StatusMethodNotAllowed,
		error: fmt.Errorf("unsupported method: %v", method),
	}
}

// NewNotImplementedError returns a StatusError whose HTTPStatusCode method
// returns http.StatusNotImplemented and whose Message method returns "not
// implemented".
func NewNotImplementedError() StatusError {
	return statusError{
		code:  http.StatusNotImplemented,
		error: fmt.Errorf("not implemented"),
	}
}

var (
	// NotFoundError is an error returned when a resource is not found.
	NotFoundError = NewBadRequestError(errors.New("not found"))
)

// FirestoreToStatusError converts an error returned from the
// "cloud.google.com/go/firestore" package to a StatusError.
func FirestoreToStatusError(err error) StatusError {
	if err, ok := err.(StatusError); ok {
		return err
	}
	if status.Code(err) == codes.NotFound {
		return NotFoundError
	}

	return NewInternalServerError(err)
}

// JSONToStatusError converts an error returned from the "encoding/json" package
// to a StatusError. It assumes that all error types defined in the
// "encoding/json" package and io.EOF are bad request errors and all others are
// internal server errors.
func JSONToStatusError(err error) StatusError {
	switch err := err.(type) {
	case StatusError:
		return err
	case *json.MarshalerError, *json.SyntaxError, *json.UnmarshalFieldError,
		*json.UnmarshalTypeError, *json.UnsupportedTypeError, *json.UnsupportedValueError:
		return NewBadRequestError(err)
	default:
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return NewBadRequestError(err)
		}
		return NewInternalServerError(err)
	}
}

// ReadCryptoRandBytes fills b with cryptographically random bytes from the
// "crypto/rand" package. It always fills all of b.
func ReadCryptoRandBytes(b []byte) {
	_, err := rand.Read(b)
	if err != nil {
		panic(fmt.Errorf("could not read random bytes: %v", err))
	}
}

// newStatusError constructs a new statusError with the given code and error.
// The given error will be used as the message returned by StatusError.Message.
func newStatusError(code int, err error) statusError {
	return statusError{
		code:  code,
		error: err,
		// Leave empty so that error.Error() will be used as the return value
		// from Message.
		message: "",
	}
}

// checkHTTPS retrieves the scheme from the X-Forwarded-Proto or RFC7239

// We do this because in the function running in a cloud container the TLS termination
// has happened upstream so we need to check the headers to reject HTTP only.
// Requests on GCE contain both of these headers and anything supplied by the client is
// overwritten. Locally in development mode we don't use HTTPS so the client should send
// one of these headers.

var (
	// De-facto standard header keys.
	xForwardedProto = http.CanonicalHeaderKey("X-Forwarded-Proto")
	forwarded       = http.CanonicalHeaderKey("Forwarded") // RFC7239

	protoRegex = regexp.MustCompile(`(?i)(?:proto=)(https|http)`)
)

func checkHTTPS(r *http.Request) StatusError {
	var scheme string

	// Retrieve the scheme from X-Forwarded-Proto.
	if proto := r.Header.Get(xForwardedProto); proto != "" {
		scheme = strings.ToLower(proto)
	} else if proto = r.Header.Get(forwarded); proto != "" {
		// match should contain at least two elements if the protocol was
		// specified in the Forwarded header. The first element will always be
		// the 'proto=' capture, which we ignore. In the case of multiple proto
		// parameters (invalid) we only extract the first.
		if match := protoRegex.FindStringSubmatch(proto); len(match) == 2 {
			scheme = strings.ToLower(match[1])
		} else if len(match) > 2 {
			return NewInternalServerError(
				fmt.Errorf("Header 'forward' has more than 2 elements"))
		}
	}

	// We want to ensure that clients always use HTTPS. Even if we don't
	// serve our API over HTTP, if clients use HTTP, they are vulnerable
	// to man-in-the-middle attacks in which the attacker communicates
	// with our service over HTTPS. In order to prevent this, it is not
	// sufficient to simply auto-upgrade to HTTPS (e.g., via a redirect
	// status code in the 300s). If we do this, then code which
	// erroneously uses HTTP will continue to work, and so it might get
	// deployed. Instead, we have to ensure that such code breaks
	// completely, alerting the code's developers to the issue, and
	// ensuring that they will change the code to use HTTPS directly.
	// Thus, we want an error code with the following properties:
	//  - Guaranteed that smart clients (such as web browsers) will not
	//    attempt to automatically upgrade to HTTPS
	//  - Doesn't have another meaning which might cause developers to
	//    overlook the error or lead them down the wrong path (e.g., if
	//    we chose 400 - bad request - they might go down the path of
	//    debugging their request format)
	//
	// For these reasons, we choose error code 418 - server is a teapot.
	// It is as unlikely as any other error code to cause the client to
	// automatically upgrade to HTTPS, and it is guaranteed to get a
	// developer's attention, hopefully getting them to look at the
	// response body, which will contain the relevant information.
	if scheme != "https" {
		return newStatusError(http.StatusTeapot,
			errors.New("unsupported protocol HTTP; only HTTPS is supported"))
	}
	return nil
}

// Add HSTS to force HTTPS usage.
// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Strict-Transport-Security
// In the following example, max-age is set to 2 years, raised from what was a former
// limit max-age of 1 year. Note that 1 year is acceptable for a domain to be included
// in browsers' HSTS preload lists. 2 years is, however, the recommended goal as a
// website's final HSTS configuration as explained on https://hstspreload.org.
// It also suffixed with preload which is necessary for inclusion in most major web
// browsers' HSTS preload lists, e.g. Chromium, Edge, & Firefox.
var headerHSTS = http.CanonicalHeaderKey("Strict-Transport-Security")

func addHSTS(w http.ResponseWriter) {
	w.Header().Set(headerHSTS, "max-age=63072000; includeSubDomains; preload")
}
