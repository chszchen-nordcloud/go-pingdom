package solarwinds

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/html"
	"io"
	"net/http"
	"net/url"
)

const (
	defaultBaseURL        = "https://my.solarwinds.cloud"
	graphQLEndpoint       = "/common/graphql"
	headerNameSetCookie   = "Set-Cookie"
	cookieNameSwicus      = "swicus"
	cookieNameSwiSettings = "swi-settings"
	headerNameCSRFToken   = "X-CSRF-Token"
)

type Client struct {
	csrfToken         string
	swiSettings       string
	email             string
	password          string
	client            *http.Client
	baseURL           string
	InvitationService *InvitationService
	UserService       *UserService
}

type ClientConfig struct {
	Username string
	Password string
	BaseURL  string
}

type Product struct {
	Name   string `json:"name"`
	Access bool   `json:"access"`
	Role   string `json:"role"`
}

type ProductUpdate struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type loginPayload struct {
	Email            string `json:"email"`
	Password         string `json:"password"`
	LoginQueryParams string `json:"loginQueryParams"`
}

type loginResult struct {
	Swicus      string
	RedirectUrl string
}

func NewClient(config ClientConfig) (*Client, error) {
	var baseURLToUse *url.URL
	var err error
	if config.BaseURL == "" {
		baseURLToUse, err = url.Parse(defaultBaseURL)
	} else {
		baseURLToUse, err = url.Parse(config.BaseURL)
	}
	if err != nil {
		return nil, err
	}
	c := &Client{
		email:    config.Username,
		password: config.Password,
		baseURL:  baseURLToUse.String(),
	}
	c.client = http.DefaultClient
	c.InvitationService = &InvitationService{client: c}
	c.UserService = &UserService{client: c}
	return c, nil
}

func (c *Client) Init() error {
	auth, err := c.login()
	if err != nil {
		return err
	}
	if err := c.obtainSwiSettings(); err != nil {
		return err
	}
	if err := c.obtainToken(auth); err != nil {
		return err
	}
	return nil
}

func (c *Client) NewRequest(method string, rsc string, params io.Reader) (*http.Request, error) {
	baseURL, err := url.Parse(c.baseURL + rsc)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(method, baseURL.String(), params)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{
		Name:  cookieNameSwiSettings,
		Value: c.swiSettings,
	})
	req.Header.Set(headerNameCSRFToken, c.csrfToken)
	return req, err
}

func (c *Client) MakeGraphQLRequest(graphQLRequest *GraphQLRequest) (*GraphQLResponse, error) {
	body, err := toJsonNoEscape(graphQLRequest)
	if err != nil {
		return nil, err
	}
	req, err := c.NewRequest("POST", graphQLEndpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	graphQLResp, err := NewGraphQLResponse(resp.Body, graphQLRequest.ResponseType)
	if err != nil {
		return nil, err
	}
	if !graphQLResp.isSuccess() {
		return nil, fmt.Errorf("request failed with message: %v", graphQLResp.message())
	}
	return graphQLResp, err
}

func (c *Client) login() (*loginResult, error) {
	params := map[string]string{
		"response_type": "code",
		"scope":         "openid swicus",
		"client_id":     "adminpanel",
		"redirect_uri":  "https://my.solarwinds.cloud/common/auth/callback",
		"state":         RandString(10),
	}
	paramsToUse := url.Values{}
	for k, v := range params {
		paramsToUse.Add(k, v)
	}
	payload := loginPayload{
		Email:            c.email,
		Password:         c.password,
		LoginQueryParams: paramsToUse.Encode(),
	}
	body, err := toJsonNoEscape(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.baseURL+"/v1/login", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(fmt.Sprintf("visit callback failed, status %v", resp.StatusCode))
	}
	defer resp.Body.Close()
	result := &loginResult{}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return nil, err
	}

	if swicus, err := retrieveCookie(resp, cookieNameSwicus); err != nil {
		return nil, err
	} else {
		result.Swicus = swicus
	}
	return result, nil
}

func (c *Client) obtainSwiSettings() error {
	resp, err := http.Get(c.baseURL + "/common/login")
	if err != nil {
		return err
	}
	if swiSettings, err := retrieveCookie(resp.Request.Response, cookieNameSwiSettings); err != nil {
		return err
	} else {
		c.swiSettings = swiSettings
	}
	return nil
}

func (c *Client) obtainToken(auth *loginResult) error {
	req, err := http.NewRequest("GET", c.baseURL+"/settings", nil)
	if err != nil {
		return err
	}
	req.AddCookie(&http.Cookie{
		Name:  cookieNameSwicus,
		Value: auth.Swicus,
	})
	req.AddCookie(&http.Cookie{
		Name:  cookieNameSwiSettings,
		Value: c.swiSettings,
	})
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("visit callback URL failed, status %d", resp.StatusCode)
	}
	doc, err := html.Parse(resp.Body)
	if err != nil {
		return err
	}
	if token, err := extractCSRFToken(doc); err != nil {
		return err
	} else {
		c.csrfToken = token
	}
	return nil
}

func extractCSRFToken(start *html.Node) (string, error) {
	var token string
	var head *html.Node
	if first := start.FirstChild; first.Type == html.DoctypeNode {
		head = first.NextSibling.FirstChild
	} else {
		head = first.FirstChild
	}
outer:
	for c := head.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "meta" && len(c.Attr) == 2 {
			for _, attr := range c.Attr {
				if attr.Key == "name" && attr.Val != "csrf-token" {
					continue outer
				}
			}
			for _, attr := range c.Attr {
				if attr.Key == "content" {
					token = attr.Val
				}
			}
			if token != "" {
				break
			}
		}
	}
	if token == "" {
		return "", errors.New("response of callback URL does not contain CSRF token")
	}
	return token, nil
}
