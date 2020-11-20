package net

import (
	"compress/gzip"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	nurl "net/url"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/zdypro888/net/http"
	"github.com/zdypro888/net/http2"
)

//HTTPDebugProxy 调试代理
var HTTPDebugProxy = &Proxy{Type: HTTP, Address: "http://127.0.0.1:8888"}

//GobalProxy 通用代理
var GobalProxy *Proxy

//Header 头
type Header = http.Header

//CookieJar cookeis
type CookieJar = http.CookieJar

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
func Request(url string, proxy *Proxy, headers Header, body io.Reader) (*Response, error) {
	return request(false, url, proxy, headers, body)
}

//RequestWithCookie Get or Post
func RequestWithCookie(url string, proxy *Proxy, headers Header, cookieJar CookieJar, body io.Reader) (*Response, error) {
	return requestWithCookie(false, url, proxy, headers, cookieJar, body)
}

//RequestMethod Http
func RequestMethod(url string, method string, proxy *Proxy, headers Header, cookieJar CookieJar, body io.Reader) (*Response, error) {
	return requestMethod(false, url, method, proxy, headers, cookieJar, body)
}

//Request2 Get or Post
func Request2(url string, proxy *Proxy, headers Header, body io.Reader) (*Response, error) {
	return request(true, url, proxy, headers, body)
}

//RequestWithCookie2 Get or Post
func RequestWithCookie2(url string, proxy *Proxy, headers Header, cookieJar CookieJar, body io.Reader) (*Response, error) {
	return requestWithCookie(true, url, proxy, headers, cookieJar, body)
}

//RequestMethod2 Http
func RequestMethod2(url string, method string, proxy *Proxy, headers Header, cookieJar CookieJar, body io.Reader) (*Response, error) {
	return requestMethod(true, url, method, proxy, headers, cookieJar, body)
}

func request(httpv2 bool, url string, proxy *Proxy, headers Header, body io.Reader) (*Response, error) {
	var method string
	if body == nil {
		method = "GET"
	} else {
		method = "POST"
	}
	return RequestMethod(url, method, proxy, headers, nil, body)
}

func requestWithCookie(httpv2 bool, url string, proxy *Proxy, headers Header, cookieJar CookieJar, body io.Reader) (*Response, error) {
	var method string
	if body == nil {
		method = "GET"
	} else {
		method = "POST"
	}
	return RequestMethod(url, method, proxy, headers, cookieJar, body)
}

func requestMethod(httpv2 bool, url string, method string, proxy *Proxy, headers Header, cookieJar CookieJar, body io.Reader) (*Response, error) {
	request, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	// request.Close = true
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
	if proxy == nil && GobalProxy != nil {
		proxy = GobalProxy
	}
	if proxy != nil {
		transport.Proxy = proxy.GetProxyURL
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   120 * time.Second,
		Jar:       cookieJar,
	}
	var redirects []*nurl.URL
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		redirects = append(redirects, req.URL)
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
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
