package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lestrrat/go-jsval"
	"github.com/stripe/stripe-mock/param"
	"github.com/stripe/stripe-mock/param/coercer"
	"github.com/stripe/stripe-mock/spec"
)

//
// Public types
//

type ErrorInfo struct {
}

// ExpansionLevel represents expansions on a single "level" of resource. It may
// have subexpansions that are meant to take effect on resources that are
// nested below it (on other levels).
type ExpansionLevel struct {
	expansions map[string]*ExpansionLevel

	// wildcard specifies that everything should be expanded.
	wildcard bool
}

// PathParamsMap holds a collection of parameter that values that have been
// extracted from the path of a request. This is useful to hand off to the data
// generator so that it can use these IDs while generating results.
type PathParamsMap struct {
	// PrimaryID contains a value for a primary ID extracted from a request
	// path. A "primary" object is the one being enacted on and which will be
	// directly returned with the API's response.
	//
	// Note that not all endpoints have a primary ID, and in those cases this
	// value will be nil. Examples of endpoints without a primary ID are
	// "create" and "list" methods.
	PrimaryID *string

	// SecondaryIDs contains a collection of "secondary IDs" (i.e., not the
	// primary ID) extracted from the request path.
	SecondaryIDs []*PathParamsSecondaryID

	// replacedPrimaryID is the old value of an ID field that's had its value
	// replaced by PrimaryID. This is used so that we can look for other
	// instances of this replaced ID, and also replace them.
	//
	// For example, if we're handling a charge and replaced an old ID `ch_old`
	// with the new value `ch_123` (from PrimaryID), this field would contain
	// `ch_old`. If we found another instance of `ch_old` in another field's
	// value (say if there was embedded refund with a field called `charge`
	// that pointed back to its parent charge ID), we'd recognize it via this
	// field and replace it with PrimaryID.
	//
	// nil if no ID has been replaced.
	replacedPrimaryID *string
}

// PathParamsSecondaryID holds the name and value for a "secondary ID" (i.e.,
// one that is not the primary ID) found in a request path.
type PathParamsSecondaryID struct {
	// ID is the value of the parameter extracted from the request path.
	ID string

	// Name is the name of the parameter according to the enclosing `{}` in the
	// OpenAPI specification.
	//
	// For example, it might read `fee` if extracted from:
	//
	//     /v1/application_fees/{fee}/refunds
	//
	Name string

	// replacedIDs is a slice of old values for an ID field that's had its
	// value replaced by this secondary parameter's new ID. This is used so
	// that we can look for other instances of this
	// replaced ID, and also replace them.
	//
	// This is a slice as opposed to a single value because it's possible that
	// we could encounter multiple fields while generating a response that all
	// represent the same entity. Say for example that a series of nested
	// expansions have been requested, each that internalizes an entity of a
	// parameter's type -- we load a fixture for each but there's no guarantee
	// that the entity in each one references the same ID.
	//
	// For more information, see PathParamsMap.replacedPrimaryID.
	replacedIDs []string
}

// appendReplacedID appends a replaced ID to the secondary ID's internal slice
// of replaced IDs.
//
// This function skips the case of an empty string value, so its use should be
// preferred over using the internal slice directly.
func (p *PathParamsSecondaryID) appendReplacedID(replacedID string) {
	if replacedID != "" {
		p.replacedIDs = append(p.replacedIDs, replacedID)
	}
}

type ResponseError struct {
	ErrorInfo struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// StubServer handles incoming HTTP requests and responds to them appropriately
// based off the set of OpenAPI routes that it's been configured with.
type StubServer struct {
	fixtures *spec.Fixtures
	routes   map[spec.HTTPVerb][]stubServerRoute
	spec     *spec.Spec
}

// HandleRequest handes an HTTP request directed at the API stub.
func (s *StubServer) HandleRequest(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	fmt.Printf("Request: %v %v\n", r.Method, r.URL.Path)

	auth := r.Header.Get("Authorization")
	if !validateAuth(auth) {
		message := fmt.Sprintf(invalidAuthorization, auth)
		stripeError := createStripeError(typeInvalidRequestError, message)
		writeResponse(w, r, start, http.StatusUnauthorized, stripeError)
		return
	}

	// We don't do anything with the idempotency key for now, but reflect it
	// back into response headers like the Stripe API does.
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey != "" {
		w.Header().Set("Idempotency-Key", idempotencyKey)
	}

	// Every response needs a Request-Id header except the invalid authorization
	w.Header().Set("Request-Id", "req_123")

	route, pathParams := s.routeRequest(r)
	if route == nil {
		message := fmt.Sprintf(invalidRoute, r.Method, r.URL.Path)
		stripeError := createStripeError(typeInvalidRequestError, message)
		writeResponse(w, r, start, http.StatusNotFound, stripeError)
		return
	}

	response, ok := route.operation.Responses["200"]
	if !ok {
		fmt.Printf("Couldn't find 200 response in spec\n")
		writeResponse(w, r, start, http.StatusInternalServerError,
			createInternalServerError())
		return
	}
	responseContent, ok := response.Content["application/json"]
	if !ok || responseContent.Schema == nil {
		fmt.Printf("Couldn't find application/json in response\n")
		writeResponse(w, r, start, http.StatusInternalServerError,
			createInternalServerError())
		return
	}

	if verbose {
		fmt.Printf("IDs extracted from route: %+v\n", pathParams)
		fmt.Printf("Response schema: %s\n", responseContent.Schema)
	}

	requestData, err := param.ParseParams(r)
	if err != nil {
		message := fmt.Sprintf("Couldn't parse query/body: %v", err)
		fmt.Printf(message + "\n")
		stripeError := createStripeError(typeInvalidRequestError, message)
		writeResponse(w, r, start, http.StatusBadRequest, stripeError)
		return
	}

	if verbose {
		if requestData != nil {
			fmt.Printf("Request data: %+v\n", requestData)
		} else {
			fmt.Printf("Request data: (none)\n")
		}
	}

	// Note that requestData is actually manipulated in place, but we show it
	// returned here to make it clear that this function will be manipulating
	// it.
	requestData, stripeError := validateAndCoerceRequest(r, route, requestData)
	if stripeError != nil {
		writeResponse(w, r, start, http.StatusBadRequest, stripeError)
		return
	}

	expansions, rawExpansions := extractExpansions(requestData)
	if verbose {
		fmt.Printf("Expansions: %+v\n", rawExpansions)
	}

	generator := DataGenerator{s.spec.Components.Schemas, s.fixtures}
	responseData, err := generator.Generate(&GenerateParams{
		Expansions:    expansions,
		PathParams:    pathParams,
		RequestData:   requestData,
		RequestMethod: r.Method,
		RequestPath:   r.URL.Path,
		Schema:        responseContent.Schema,
	})
	if err != nil {
		fmt.Printf("Couldn't generate response: %v\n", err)
		writeResponse(w, r, start, http.StatusInternalServerError,
			createInternalServerError())
		return
	}
	if verbose {
		responseDataJson, err := json.MarshalIndent(responseData, "", "  ")
		if err != nil {
			panic(err)
		}
		fmt.Printf("Response data: %s\n", responseDataJson)
	}
	writeResponse(w, r, start, http.StatusOK, responseData)
}

func (s *StubServer) initializeRouter() error {
	var numEndpoints int
	var numPaths int
	var numValidators int

	s.routes = make(map[spec.HTTPVerb][]stubServerRoute)

	componentsForValidation := spec.GetComponentsForValidation(&s.spec.Components)

	for path, verbs := range s.spec.Paths {
		numPaths++

		pathPattern, pathParamNames := compilePath(path)

		if verbose {
			fmt.Printf("Compiled path: %v\n", pathPattern.String())
		}

		for verb, operation := range verbs {
			numEndpoints++

			_, requestBodySchema := getRequestBodySchema(operation)
			var requestBodyValidator *jsval.JSVal
			if requestBodySchema != nil {
				var err error
				requestBodyValidator, err = spec.GetValidatorForOpenAPI3Schema(
					requestBodySchema, componentsForValidation)
				if err != nil {
					return err
				}
			}

			// Note that this may be nil if no suitable validator could be
			// generated.
			if requestBodyValidator != nil {
				numValidators++
			}

			// We use whether the route ends with a parameter as a heuristic as
			// to whether we should expect an object's primary ID in the URL.
			var hasPrimaryID bool
			for _, suffix := range hasPrimaryIDSuffixes {
				if strings.HasSuffix(string(path), suffix) {
					hasPrimaryID = true
					break
				}
			}

			route := stubServerRoute{
				hasPrimaryID:         hasPrimaryID,
				pattern:              pathPattern,
				operation:            operation,
				pathParamNames:       pathParamNames,
				requestBodyValidator: requestBodyValidator,
			}

			// net/http will always give us verbs in uppercase, so build our
			// routing table this way too
			verb = spec.HTTPVerb(strings.ToUpper(string(verb)))

			s.routes[verb] = append(s.routes[verb], route)
		}
	}

	fmt.Printf("Routing to %v path(s) and %v endpoint(s) with %v validator(s)\n",
		numPaths, numEndpoints, numValidators)
	return nil
}

// routeRequest tries to find a matching route for the given request. If
// successful, it returns the matched route and where possible, an extracted ID
// which comes from the last capture group in the URL. An ID is only returned
// if it looks like it's supposed to be the primary identifier of the returned
// object (i.e., the route's pattern ended with a parameter). A nil is returned
// as the second return value when no primary ID is available.
func (s *StubServer) routeRequest(r *http.Request) (*stubServerRoute, *PathParamsMap) {
	verbRoutes := s.routes[spec.HTTPVerb(r.Method)]
	for _, route := range verbRoutes {
		matches := route.pattern.FindAllStringSubmatch(r.URL.Path, -1)

		if len(matches) < 1 {
			continue
		}

		// There are no path parameters. Return the route only.
		if len(route.pathParamNames) < 1 {
			return &route, nil
		}

		// There will only ever be a single match in the string (this match
		// contains the entire match plus all capture groups).
		firstMatch := matches[0]

		// Secondary IDs are any IDs in the URL that are *not* the primary ID
		// (which you'll see if say a resource is nested under another
		// resource).
		//
		// Normally, we can calculate the number of secondary IDs based on the
		// number of path parameters by subtracting one for the primary ID.
		// There's a special case if the path doesn't have a primary ID in
		// which the number of secondary IDs equals the number of path
		// parameters.
		var numSecondaryIDs int
		if route.hasPrimaryID {
			numSecondaryIDs = len(route.pathParamNames) - 1
		} else {
			numSecondaryIDs = len(route.pathParamNames)
		}

		var secondaryIDs []*PathParamsSecondaryID
		if numSecondaryIDs > 0 {
			secondaryIDs = make([]*PathParamsSecondaryID, numSecondaryIDs)
			for i := 0; i < numSecondaryIDs; i++ {
				secondaryIDs[i] = &PathParamsSecondaryID{
					// Note that the first position of `firstMatch` is the
					// entire matching string. Capture groups start at position
					// 1, so we add one to `i`.
					ID: firstMatch[i+1],

					Name: route.pathParamNames[i],
				}
			}
		}

		// Not all routes have a primary ID even if they might have secondary
		// IDs. Consider for example a list endpoint nested under another
		// resource:
		//
		//     GET "/v1/application_fees/fee_123/refunds
		//
		var primaryID *string
		if route.hasPrimaryID {
			primaryID = &firstMatch[len(firstMatch)-1]
		}

		// Return the route along with any IDs that matched in the path.
		return &route, &PathParamsMap{
			PrimaryID:    primaryID,
			SecondaryIDs: secondaryIDs,
		}
	}
	return nil, nil
}

//
// Private values
//

const (
	contentTypeEmpty      = "Request's `Content-Type` header was empty. Expected: `%s`."
	contentTypeMismatched = "Request's `Content-Type` didn't match the path's expected media type. Expected: `%s`. Was: `%s`."

	invalidAuthorization = "Please authenticate by specifying an " +
		"`Authorization` header with any valid looking testmode secret API " +
		"key. For example, `Authorization: Bearer sk_test_123`. " +
		"Authorization was '%s'."

	invalidRoute = "Unrecognized request URL (%s: %s)."

	internalServerError = "An internal error occurred."

	typeInvalidRequestError = "invalid_request_error"
)

// Suffixes for which we will try to exact an object's ID from the path.
var hasPrimaryIDSuffixes = [...]string{
	// The general case: we're looking for the end of an OpenAPI URL parameter.
	"}",

	// These are resource "actions". They don't take the standard form, but we
	// can expect an object's primary ID to live right before them in a path.
	"/approve",
	"/capture",
	"/cancel",
	"/close",
	"/decline",
	"/pay",
	"/refund",
	"/reject",
	"/verify",
}

var pathParameterPattern = regexp.MustCompile(`\{(\w+)\}`)

//
// Private types
//

// stubServerRoute is a single route in a StubServer's routing table. It has a
// pattern to match an incoming path and a description of the method that would
// be executed in the event of a match.
type stubServerRoute struct {
	hasPrimaryID         bool
	pattern              *regexp.Regexp
	operation            *spec.Operation
	pathParamNames       []string
	requestBodyValidator *jsval.JSVal
}

//
// Private functions
//

// compilePath compiles a path extracted from OpenAPI into a regular expression
// that we can use for matching against incoming HTTP requests.
//
// The first return value is a regular expression. The second is a slice of
// names for the parameters included in the path in order of their appearance.
// This slice is `nil` if the path had no parameters.
func compilePath(path spec.Path) (*regexp.Regexp, []string) {
	var pathParamNames []string
	parts := strings.Split(string(path), "/")
	pattern := `\A`

	for _, part := range parts {
		if part == "" {
			continue
		}

		submatches := pathParameterPattern.FindAllStringSubmatch(part, -1)
		if submatches == nil {
			pattern += `/` + part
		} else {
			pattern += `/(?P<` + submatches[0][1] + `>[\w-_.]+)`
			pathParamNames = append(pathParamNames, submatches[0][1])
		}
	}

	return regexp.MustCompile(pattern + `\z`), pathParamNames
}

// Helper to create an internal server error for API issues.
func createInternalServerError() *ResponseError {
	return createStripeError(typeInvalidRequestError, internalServerError)
}

// This creates a Stripe error to return in case of API errors.
func createStripeError(errorType string, errorMessage string) *ResponseError {
	return &ResponseError{
		ErrorInfo: struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}{
			Message: errorMessage,
			Type:    errorType,
		},
	}
}

func extractExpansions(data map[string]interface{}) (*ExpansionLevel, []string) {
	expand, ok := data["expand"]
	if !ok {
		return nil, nil
	}

	var expansions []string

	expandStr, ok := expand.(string)
	if ok {
		expansions = append(expansions, expandStr)
		return parseExpansionLevel(expansions), expansions
	}

	expandArr, ok := expand.([]interface{})
	if ok {
		for _, expand := range expandArr {
			expandStr := expand.(string)
			expansions = append(expansions, expandStr)
		}
		return parseExpansionLevel(expansions), expansions
	}

	return nil, nil
}

// getRequestBodySchema gets the media type and expected request schema for the
// given operation. We don't expect any endpoint in the Stripe API to have
// multiple supported media types, so the operation's first media type and
// request schema is always the one that's returned.
//
// The first value is a media type like "application/x-www-form-urlencoded", or
// nil if the operation has no request schemas.
func getRequestBodySchema(operation *spec.Operation) (*string, *spec.Schema) {
	if operation.RequestBody == nil {
		return nil, nil
	}

	for mediaType, spec := range operation.RequestBody.Content {
		return &mediaType, spec.Schema
	}

	return nil, nil
}

func isCurl(userAgent string) bool {
	return strings.HasPrefix(userAgent, "curl/")
}

// parseExpansionLevel parses a set of raw expansions from a request query
// string or form and produces a structure more useful for performing actual
// expansions.
func parseExpansionLevel(raw []string) *ExpansionLevel {
	sort.Strings(raw)

	level := &ExpansionLevel{expansions: make(map[string]*ExpansionLevel)}
	groups := make(map[string][]string)

	for _, expansion := range raw {
		parts := strings.Split(expansion, ".")
		if len(parts) == 1 {
			if parts[0] == "*" {
				level.wildcard = true
			} else {
				level.expansions[parts[0]] =
					&ExpansionLevel{expansions: make(map[string]*ExpansionLevel)}
			}
		} else {
			groups[parts[0]] = append(groups[parts[0]], strings.Join(parts[1:], "."))
		}
	}

	for key, subexpansions := range groups {
		level.expansions[key] = parseExpansionLevel(subexpansions)
	}

	return level
}

// validateAndCoerceRequest validates an incoming request against an OpenAPI
// schema and does parameter coercion.
//
// Firstly, `Content-Type` is checked against the schema's media type, then
// string-encoded parameters are coerced to expected types (where possible).
// Finally, we validate the incoming payload against the schema.
func validateAndCoerceRequest(
	r *http.Request,
	route *stubServerRoute,
	requestData map[string]interface{}) (map[string]interface{}, *ResponseError) {

	// Currently we only validate parameters in the request body, but we should
	// really validate query and URL parameters as well now that we've
	// transitioned to OpenAPI 3.0.
	mediaType, bodySchema := getRequestBodySchema(route.operation)

	// There are no parameters on this route to validate.
	if mediaType == nil {
		return requestData, nil
	}

	contentType := r.Header.Get("Content-Type")

	if contentType == "" {
		// A special case: if a `DELETE` operation comes in with no request
		// payload, we allow it. Most `DELETE` operations take no parameters,
		// but a few of them take some optional ones.
		if r.Method == http.MethodDelete {
			return requestData, nil
		}

		message := fmt.Sprintf(contentTypeEmpty, *mediaType)
		fmt.Printf(message + "\n")
		return nil, createStripeError(typeInvalidRequestError, message)
	}

	// Truncate content type parameters. For example, given:
	//
	//     application/json; charset=utf-8
	//
	// We want to chop off the `; charset=utf-8` at the end.
	contentType = strings.Split(contentType, ";")[0]

	if contentType != *mediaType {
		message := fmt.Sprintf(contentTypeMismatched, *mediaType, contentType)
		fmt.Printf(message + "\n")
		return nil, createStripeError(typeInvalidRequestError, message)
	}

	err := coercer.CoerceParams(bodySchema, requestData)
	if err != nil {
		message := fmt.Sprintf("Request coercion error: %v", err)
		fmt.Printf(message + "\n")
		return nil, createStripeError(typeInvalidRequestError, message)
	}

	fmt.Printf("Request data = %+v\n", requestData)
	err = route.requestBodyValidator.Validate(requestData)
	if err != nil {
		message := fmt.Sprintf("Request validation error: %v", err)
		fmt.Printf(message + "\n")
		return nil, createStripeError(typeInvalidRequestError, message)
	}

	// All checks were successful.
	return requestData, nil
}

func validateAuth(auth string) bool {
	if auth == "" {
		return false
	}

	parts := strings.Split(auth, " ")

	// Expect ["Bearer", "sk_test_123"] or ["Basic", "aaaaa"]
	if len(parts) != 2 || parts[1] == "" {
		return false
	}

	var key string
	switch parts[0] {
	case "Basic":
		keyBytes, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return false
		}
		key = string(keyBytes)

	case "Bearer":
		key = parts[1]

	default:
		return false
	}

	keyParts := strings.Split(key, "_")

	// Expect ["sk", "test", "123"]
	if len(keyParts) != 3 {
		return false
	}

	if keyParts[0] != "sk" {
		return false
	}

	if keyParts[1] != "test" {
		return false
	}

	// Expect something (anything but an empty string) in the third position
	if len(keyParts[2]) == 0 {
		return false
	}

	return true
}

func writeResponse(w http.ResponseWriter, r *http.Request, start time.Time, status int, data interface{}) {
	if data == nil {
		data = http.StatusText(status)
	}

	var encodedData []byte
	var err error

	if !isCurl(r.Header.Get("User-Agent")) {
		encodedData, err = json.Marshal(&data)
	} else {
		encodedData, err = json.MarshalIndent(&data, "", "  ")
		encodedData = append(encodedData, '\n')
	}

	if err != nil {
		fmt.Printf("Error serializing response: %v\n", err)
		writeResponse(w, r, start, http.StatusInternalServerError, nil)
		return
	}

	w.Header().Set("Stripe-Mock-Version", version)

	w.WriteHeader(status)
	_, err = w.Write(encodedData)
	if err != nil {
		fmt.Printf("Error writing to client: %v\n", err)
	}
	fmt.Printf("Response: elapsed=%v status=%v\n", time.Now().Sub(start), status)
}
