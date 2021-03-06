package sand

import (
	"crypto/tls"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/coupa/sand-go/cache"

	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

//Client can be used to request token from an OAuth2 server
type Client struct {
	//The client ID of the OAuth2 client credentials
	ClientID string
	//The client secret of the OAuth2 client credentials
	ClientSecret string
	//TokenURL: The token endpoint of the OAuth2 server, e.g., "https://oauth.example.com/oauth2/token"
	TokenURL string

	//SkipTLSVerify skips checking the SSL certificate. Should be false for production.
	//Default is false
	SkipTLSVerify bool

	//MaxRetry is the maximum number of retries to perform with exponential backoff
	//when connecting to the OAuth2 server fails.
	//Default value is 5
	MaxRetry int
	Cache    cache.Cache

	//CacheRoot is the root of the cache key for storing tokens in the cache.
	//The overall cache key will look like: <CacheRoot>/<cacheType>/<some key>
	//Default value is "sand"
	CacheRoot string

	//Default value is "resources" for sand.Client
	//Default value is "tokens" for sand.Service
	cacheType string
}

//NewClient returns a Client with default option values.
func NewClient(id, secret, tokenURL string) (client *Client, err error) {
	if id == "" || secret == "" || tokenURL == "" {
		err = errors.New("NewClient: missing required argument(s)")
		return
	}
	client = &Client{
		ClientID:      id,
		ClientSecret:  secret,
		TokenURL:      tokenURL,
		SkipTLSVerify: false,
		MaxRetry:      5,
		Cache:         nil,
		CacheRoot:     "sand",
		cacheType:     "resources",
	}
	return
}

//Request makes a service API request by first obtaining the access token from
//SAND. Then it deligates the token to the underlying function to make the service
//call. If the service returns 401, it performs exponential retry by requesting
//new tokens from SAND and make the service call. If the service returns 502, the
//service failed to connect to the authentication service and no retry will occur.
//Usage Example:
// client.Request("some-service", []string{"s1", "s2"}, func(token string) (*http.Response, error) {
//   // Make http request with "Bearer {token}" in the Authorization header
//   // return the response and error
// })
func (c *Client) Request(cacheKey string, scopes []string, exec func(string) (*http.Response, error)) (*http.Response, error) {
	return c.RequestWithCustomRetry(cacheKey, scopes, c.MaxRetry, exec)
}

//RequestWithCustomRetry allows specifying numRetry as the number of retries to
//use instead of the default MaxRetry, on a per-request basis.
//Using a negative number for numRetry is equivalent to the "Request" function which uses MaxRetry.
//The retry durations are: 1, 2, 4, 8, 16,... seconds
func (c *Client) RequestWithCustomRetry(cacheKey string, scopes []string, numRetry int, exec func(string) (*http.Response, error)) (*http.Response, error) {
	if numRetry < 0 {
		numRetry = c.MaxRetry
	}
	token, err := c.Token(cacheKey, scopes, numRetry)
	if err != nil {
		return nil, err
	}
	resp, err := exec(token)
	if err != nil {
		return resp, err
	}
	if numRetry > 0 {
		//Retry only on 401 response from the service.
		//Get a fresh token from authentication service and retry.
		for retry := 0; resp.StatusCode == http.StatusUnauthorized && retry < numRetry; retry++ {
			sleep := time.Duration(math.Pow(2, float64(retry)))
			logger.Warnf("Sand request: retrying after %d sec on %d", sleep, http.StatusUnauthorized)
			time.Sleep(sleep * time.Second)
			//Prevent reading from cache on retry
			if c.Cache != nil {
				c.Cache.Delete(c.cacheKey(cacheKey, scopes))
			}
			//Set number of retry to 0, since we are already retrying here, don't retry
			//when getting the token. Otherwise it may lock up for a long time
			token, err = c.Token(cacheKey, scopes, 0)
			if err != nil {
				return resp, err
			}
			resp, err = exec(token)
			if err != nil {
				return resp, err
			}
		}
	}
	return resp, err
}

//Token returns an OAuth token retrieved from the OAuth2 server. It also puts the
//token in the cache up to specified amount of time.
func (c *Client) Token(cacheKey string, scopes []string, numRetry int) (string, error) {
	if c.Cache != nil && cacheKey != "" {
		token := c.Cache.Read(c.cacheKey(cacheKey, scopes))
		if token != nil {
			return token.(string), nil
		}
	}
	token, err := c.oauthToken(scopes, numRetry)
	if err != nil {
		return "", err
	}
	if token.AccessToken == "" {
		return "", AuthenticationError{"Invalid access token"}
	}
	if c.Cache != nil && cacheKey != "" {
		expiresIn := 0
		//If token.Expiry is zero, it means no limit. Otherwise we compute the limit.
		if !token.Expiry.IsZero() {
			expiresIn = int(token.Expiry.Unix() - time.Now().Unix())
		}
		if expiresIn >= 0 {
			c.Cache.Write(c.cacheKey(cacheKey, scopes), token.AccessToken, time.Duration(expiresIn)*time.Second)
		}
	}
	return token.AccessToken, nil
}

//oauthToken makes the connection to the OAuth server and returns oauth2.Token
//The returned token could have empty accessToken.
func (c *Client) oauthToken(scopes []string, numRetry int) (token *oauth2.Token, err error) {
	if numRetry < 0 {
		numRetry = c.MaxRetry
	}
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: c.SkipTLSVerify},
	}}
	ctx := context.TODO()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)

	config := clientcredentials.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		TokenURL:     c.TokenURL,
		Scopes:       scopes,
	}
	token, err = config.Token(ctx)
	if err != nil && numRetry > 0 {
		for retry := 0; err != nil && retry < numRetry; retry++ {
			//Exponential backoff on the retry
			sleep := time.Duration(math.Pow(2, float64(retry)))
			logger.Warnf("Sand token: retrying after %d sec because of error: %v", sleep, err)
			time.Sleep(sleep * time.Second)
			token, err = config.Token(ctx)
		}
	}
	if err != nil {
		err = AuthenticationError{err.Error()}
	}
	return token, err
}

//cacheKey builds the cache key in the format: <CachRoot>/<cacheType>/<key>
func (c *Client) cacheKey(key string, scopes []string) string {
	rv := c.CacheRoot + "/" + c.cacheType + "/" + key
	if len(scopes) > 0 {
		rv += "/" + strings.Join(scopes, "_")
	}
	return rv
}
