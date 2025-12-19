package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const (
	tokenURL        = "https://sso.redhat.com/auth/realms/redhat-external/protocol/openid-connect/token"
	imageBuilderURL = "https://console.redhat.com/api/image-builder/v1"
	pollInterval    = 30 * time.Second
	buildTimeout    = 2 * time.Hour
	tokenBuffer     = 60 * time.Second // refresh token 60s before expiry
)

type TokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
}

type TokenManager struct {
	mu           sync.Mutex
	token        string
	expiresAt    time.Time
	clientID     string
	clientSecret string
	offlineToken string
}

func NewTokenManager(clientID, clientSecret, offlineToken string) *TokenManager {
	return &TokenManager{
		clientID:     clientID,
		clientSecret: clientSecret,
		offlineToken: offlineToken,
	}
}

func (tm *TokenManager) GetToken() (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.token != "" && time.Now().Add(tokenBuffer).Before(tm.expiresAt) {
		return tm.token, nil
	}

	var token string
	var expiresIn int
	var err error

	if tm.offlineToken != "" {
		token, expiresIn, err = requestTokenWithOfflineToken(tm.offlineToken)
	} else {
		token, expiresIn, err = requestTokenWithClientCredentials(tm.clientID, tm.clientSecret)
	}

	if err != nil {
		return "", err
	}

	tm.token = token
	tm.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)

	return tm.token, nil
}

func requestTokenWithClientCredentials(clientID, clientSecret string) (string, int, error) {
	data := url.Values{}
	data.Set("grant_type", "client_credentials")
	data.Set("client_id", clientID)
	data.Set("client_secret", clientSecret)

	return requestToken(data)
}

func requestTokenWithOfflineToken(offlineToken string) (string, int, error) {
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("client_id", "rhsm-api")
	data.Set("refresh_token", offlineToken)

	return requestToken(data)
}

func requestToken(data url.Values) (string, int, error) {
	resp, err := http.PostForm(tokenURL, data)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token request failed: %s - %s", resp.Status, string(body))
	}

	var tokenResp TokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", 0, err
	}

	return tokenResp.AccessToken, tokenResp.ExpiresIn, nil
}

func doRequest(tokenManager *TokenManager, method, urlStr string, body []byte) (*http.Response, []byte, error) {
	token, err := tokenManager.GetToken()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get token: %w", err)
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, urlStr, bodyReader)
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "curl/8.15.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, nil, err
	}

	return resp, respBody, nil
}

// Blueprint types
type CreateBlueprintRequest struct {
	Name           string                  `json:"name"`
	Description    string                  `json:"description"`
	Distribution   string                  `json:"distribution"`
	ImageRequests  []BlueprintImageRequest `json:"image_requests"`
	Customizations map[string]any          `json:"customizations"`
}

type BlueprintImageRequest struct {
	Architecture  string                 `json:"architecture"`
	ImageType     string                 `json:"image_type"`
	UploadRequest BlueprintUploadRequest `json:"upload_request"`
}

type BlueprintUploadRequest struct {
	Type    string         `json:"type"`
	Options map[string]any `json:"options"`
}

type CreateBlueprintResponse struct {
	ID string `json:"id"`
}

type ComposeBlueprintResponse struct {
	ID string `json:"id"`
}

type ComposeStatusResponse struct {
	ImageStatus ImageStatus `json:"image_status"`
}

type ImageStatus struct {
	Status string              `json:"status"`
	Error  *ComposeStatusError `json:"error,omitempty"`
}

type ComposeStatusError struct {
	Reason string `json:"reason"`
}

func createBlueprint(tokenManager *TokenManager, req CreateBlueprintRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}

	resp, respBody, err := doRequest(tokenManager, "POST", imageBuilderURL+"/blueprints", body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s - %s", resp.Status, string(respBody))
	}

	var blueprintResp CreateBlueprintResponse
	if err := json.Unmarshal(respBody, &blueprintResp); err != nil {
		return "", err
	}

	return blueprintResp.ID, nil
}

func composeFromBlueprint(tokenManager *TokenManager, blueprintID string) (string, error) {
	resp, respBody, err := doRequest(tokenManager, "POST", imageBuilderURL+"/blueprints/"+blueprintID+"/compose", nil)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s - %s", resp.Status, string(respBody))
	}

	var composeResp []ComposeBlueprintResponse
	if err := json.Unmarshal(respBody, &composeResp); err != nil {
		return "", err
	}

	if len(composeResp) == 0 {
		return "", fmt.Errorf("no compose ID returned")
	}

	return composeResp[0].ID, nil
}

func deleteBlueprint(tokenManager *TokenManager, blueprintID string) error {
	_, _, err := doRequest(tokenManager, "DELETE", imageBuilderURL+"/blueprints/"+blueprintID, nil)
	return err
}

func waitForCompose(tokenManager *TokenManager, composeID string) (bool, error) {
	deadline := time.Now().Add(buildTimeout)

	for time.Now().Before(deadline) {
		imageStatus, err := getComposeStatus(tokenManager, composeID)
		if err != nil {
			return false, err
		}

		switch imageStatus.Status {
		case "success":
			return true, nil
		case "failure":
			errReason := "unknown reason"
			if imageStatus.Error != nil && imageStatus.Error.Reason != "" {
				errReason = imageStatus.Error.Reason
			}
			return false, fmt.Errorf("%s", errReason)
		case "pending", "building", "uploading", "registering":
			time.Sleep(pollInterval)
		default:
			return false, fmt.Errorf("unknown status: %s", imageStatus.Status)
		}
	}

	return false, fmt.Errorf("timeout after %v", buildTimeout)
}

func getComposeStatus(tokenManager *TokenManager, composeID string) (ImageStatus, error) {
	resp, body, err := doRequest(tokenManager, "GET", imageBuilderURL+"/composes/"+composeID, nil)
	if err != nil {
		return ImageStatus{}, err
	}

	if resp.StatusCode != http.StatusOK {
		return ImageStatus{}, fmt.Errorf("status request failed: %s - %s", resp.Status, string(body))
	}

	var statusResp ComposeStatusResponse
	if err := json.Unmarshal(body, &statusResp); err != nil {
		return ImageStatus{}, err
	}

	return statusResp.ImageStatus, nil
}
