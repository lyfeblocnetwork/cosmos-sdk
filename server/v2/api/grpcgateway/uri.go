package grpcgateway

import (
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"

	"github.com/cosmos/gogoproto/jsonpb"
	gogoproto "github.com/cosmos/gogoproto/proto"
	"github.com/mitchellh/mapstructure"
)

const MaxBodySize = 1 << 20 // 1 MB

// URIMatch contains information related to a URI match.
type URIMatch struct {
	// QueryInputName is the fully qualified name of the proto input type of the query rpc method.
	QueryInputName string

	// Params are any wildcard params found in the request.
	//
	// example: foo/bar/{baz} - foo/bar/qux -> {baz: qux}
	Params map[string]string
}

// HasParams reports whether the URIMatch has any params.
func (uri URIMatch) HasParams() bool {
	return len(uri.Params) > 0
}

// matchURI attempts to find a match for the given URI.
// NOTE: if no match is found, nil is returned.
func matchURI(uri string, getPatternToQueryInputName map[string]string) *URIMatch {
	uri = strings.TrimRight(uri, "/")

	// for simple cases where there are no wildcards, we can just do a map lookup.
	if inputName, ok := getPatternToQueryInputName[uri]; ok {
		return &URIMatch{
			QueryInputName: inputName,
		}
	}

	// attempt to find a match in the pattern map.
	for getPattern, queryInputName := range getPatternToQueryInputName {
		getPattern = strings.TrimRight(getPattern, "/")

		regexPattern, wildcardNames := patternToRegex(getPattern)

		regex := regexp.MustCompile(regexPattern)
		matches := regex.FindStringSubmatch(uri)

		if matches != nil && len(matches) > 1 {
			// first match is the full string, subsequent matches are capture groups
			params := make(map[string]string)
			for i, name := range wildcardNames {
				params[name] = matches[i+1]
			}

			return &URIMatch{
				QueryInputName: queryInputName,
				Params:         params,
			}
		}
	}

	return nil
}

// patternToRegex converts a URI pattern with wildcards to a regex pattern.
// Returns the regex pattern and a slice of wildcard names in order
func patternToRegex(pattern string) (string, []string) {
	escaped := regexp.QuoteMeta(pattern)
	var wildcardNames []string

	// extract and replace {param=**} patterns
	r1 := regexp.MustCompile(`\\\{([^}]+?)=\\\*\\\*\\}`)
	escaped = r1.ReplaceAllStringFunc(escaped, func(match string) string {
		// extract wildcard name without the =** suffix
		name := regexp.MustCompile(`\\\{(.+?)=`).FindStringSubmatch(match)[1]
		wildcardNames = append(wildcardNames, name)
		return "(.+)"
	})

	// extract and replace {param} patterns
	r2 := regexp.MustCompile(`\\\{([^}]+)\\}`)
	escaped = r2.ReplaceAllStringFunc(escaped, func(match string) string {
		// extract wildcard name from the curl braces {}.
		name := regexp.MustCompile(`\\\{(.*?)\\}`).FindStringSubmatch(match)[1]
		wildcardNames = append(wildcardNames, name)
		return "([^/]+)"
	})

	return "^" + escaped + "$", wildcardNames
}

// createMessageFromJSON creates a message from the URIMatch given the JSON body in the http request.
func createMessageFromJSON(match *URIMatch, r *http.Request) (gogoproto.Message, error) {
	requestType := gogoproto.MessageType(match.QueryInputName)
	if requestType == nil {
		return nil, fmt.Errorf("unknown request type")
	}

	msg, ok := reflect.New(requestType.Elem()).Interface().(gogoproto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to create message instance")
	}

	defer r.Body.Close()
	limitedReader := io.LimitReader(r.Body, MaxBodySize)
	err := jsonpb.Unmarshal(limitedReader, msg)
	if err != nil {
		return nil, fmt.Errorf("error parsing body: %w", err)
	}

	return msg, nil

}

// createMessage creates a message from the given URIMatch. If the match has params, the message will be populated
// with the value of those params. Otherwise, an empty message is returned.
func createMessage(match *URIMatch) (gogoproto.Message, error) {
	requestType := gogoproto.MessageType(match.QueryInputName)
	if requestType == nil {
		return nil, fmt.Errorf("unknown request type")
	}

	msg, ok := reflect.New(requestType.Elem()).Interface().(gogoproto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to create message instance")
	}

	// if the uri match has params, we need to populate the message with the values of those params.
	if match.HasParams() {
		// create a map with the proper field names from protobuf tags
		fieldMap := make(map[string]string)
		v := reflect.ValueOf(msg).Elem()
		t := v.Type()

		for key, value := range match.Params {
			// attempt to match wildcard name to protobuf struct tag.
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				tag := field.Tag.Get("protobuf")
				if nameMatch := regexp.MustCompile(`name=(\w+)`).FindStringSubmatch(tag); len(nameMatch) > 1 {
					if nameMatch[1] == key {
						fieldMap[field.Name] = value
						break
					}
				}
			}
		}

		decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
			Result:           msg,
			WeaklyTypedInput: true, // TODO(technicallyty): should we put false here?
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create decoder: %w", err)
		}

		if err := decoder.Decode(fieldMap); err != nil {
			return nil, fmt.Errorf("failed to decode params: %w", err)
		}
	}
	return msg, nil
}
