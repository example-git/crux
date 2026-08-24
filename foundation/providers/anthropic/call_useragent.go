// Modified by the Crux project for in-repository integration.
package anthropic

import (
	"github.com/anthropics/anthropic-sdk-go/option"
	fantasy "github.com/example-git/crux/foundation"
)

func callUARequestOptions(call fantasy.Call) []option.RequestOption {
	if ua, ok := fantasy.CallUserAgent(call.UserAgent); ok {
		return []option.RequestOption{option.WithHeader("User-Agent", ua)}
	}
	return nil
}

func callHeadersRequestOptions(call fantasy.Call) []option.RequestOption {
	headers, ok := fantasy.CallHeaders(call.Headers)
	if !ok {
		return nil
	}
	opts := make([]option.RequestOption, 0, len(headers))
	for k, v := range headers {
		opts = append(opts, option.WithHeader(k, v))
	}
	return opts
}
