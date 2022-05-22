package net

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	nurl "net/url"
	"time"

	"github.com/andybalholm/brotli"
	"golang.org/x/net/http2"
)

//Response 请求返回
type Response struct {
	Code      int
	Header    http.Header
	Redirects []*nurl.URL
	Body      []byte
}

func (res *Response) Error() string {
	return fmt.Sprintf("response status code: %d", res.Code)
}

//Request Get or Post
func Request(ctx context.Context, url string, headers http.Header, body io.Reader) (*Response, error) {
	return request(ctx, false, url, headers, body)
}

//RequestMethod Http
func RequestMethod(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	return requestMethod(ctx, false, url, method, headers, body)
}

//Request2 Get or Post
func Request2(ctx context.Context, url string, headers http.Header, body io.Reader) (*Response, error) {
	return request(ctx, true, url, headers, body)
}

//RequestMethod2 Http
func RequestMethod2(ctx context.Context, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	return requestMethod(ctx, true, url, method, headers, body)
}

func request(ctx context.Context, httpv2 bool, url string, headers http.Header, body io.Reader) (*Response, error) {
	var method string
	if body == nil {
		method = "GET"
	} else {
		method = "POST"
	}
	return requestMethod(ctx, httpv2, url, method, headers, body)
}

func requestMethod(ctx context.Context, httpv2 bool, url string, method string, headers http.Header, body io.Reader) (*Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		request.Header = http.Header(headers).Clone()
	}
	transport := &http.Transport{
		Dial: (&net.Dialer{
			Timeout:   20 * time.Second,
			KeepAlive: 0,
		}).Dial,
		ResponseHeaderTimeout: 20 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			MinVersion:         tls.VersionTLS11,
			MaxVersion:         tls.VersionTLS13,
		},
	}
	if httpv2 {
		if err = http2.ConfigureTransport(transport); err != nil {
			return nil, err
		}
	}
	if proxyContext, ok := ctx.(ProxyContext); ok {
		transport.Proxy = proxyContext.GetProxyURL
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second,
	}
	if jctx, ok := ctx.(CookieContext); ok {
		if cookieJar := jctx.GetCookie(url); cookieJar != nil {
			client.Jar = cookieJar
		}
	}
	var redirects []*nurl.URL
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		redirects = append(redirects, req.URL)
		return nil
	}
	var response *http.Response
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if response, err = client.Do(request); err != nil {
			if proxyContext, ok := ctx.(ProxyContext); !ok {
				return nil, err
			} else if err = proxyContext.GetProxyError(err); err != nil {
				return nil, err
			}
		} else if responseContext, ok := ctx.(ResponseContext); ok {
			if response, err = responseContext.Do(response); err != nil {
				return nil, err
			} else if response != nil {
				break
			}
		} else {
			break
		}
	}
	defer response.Body.Close()
	data, err := ReadResponse(response)
	return &Response{
		Code:      response.StatusCode,
		Header:    response.Header,
		Redirects: redirects,
		Body:      data,
	}, err
}

//ReadResponse 读取 response body
func ReadResponse(response *http.Response) ([]byte, error) {
	var err error
	var body []byte
	switch response.Header.Get("Content-Encoding") {
	case "gzip":
		var reader *gzip.Reader
		reader, err = gzip.NewReader(response.Body)
		if err != nil {
			return nil, err
		}
		body, err = ioutil.ReadAll(reader)
	case "br":
		reader := brotli.NewReader(response.Body)
		body, err = ioutil.ReadAll(reader)
	default:
		body, err = ioutil.ReadAll(response.Body)
	}
	return body, err
}
