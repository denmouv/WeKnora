package wechat

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/google/uuid"
)

// Compile-time check that Adapter implements im.FileUploader.
var _ im.FileUploader = (*Adapter)(nil)

// UploadFile uploads a file to the iLink CDN for delivery to a WeChat user.
//
// The iLink upload flow:
//  1. Generate a random AES-128 key.
//  2. Encrypt the file with AES-128-ECB.
//  3. Call getuploadurl to obtain a CDN upload URL.
//  4. HTTP PUT the encrypted bytes to the CDN.
//  5. Return the encrypt_query_param and aes_key for use in sendmessage.
func (a *Adapter) UploadFile(
	ctx context.Context,
	incoming *im.IncomingMessage,
	fileName string,
	fileData []byte,
) (*im.UploadedFileRef, error) {
	if len(fileData) == 0 {
		return nil, fmt.Errorf("file data is empty")
	}

	toUserID := incoming.UserID
	if toUserID == "" {
		return nil, fmt.Errorf("no recipient user ID")
	}

	// 1. Generate AES-128 key (16 random bytes).
	aesKey := make([]byte, 16)
	if _, err := rand.Read(aesKey); err != nil {
		return nil, fmt.Errorf("generate aes key: %w", err)
	}

	// 2. Encrypt with AES-128-ECB (reuses crypto.go).
	encrypted, err := encryptAES128ECB(fileData, aesKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt file: %w", err)
	}

	// 3. Compute MD5 hashes.
	rawMD5 := md5.Sum(fileData)
	encMD5 := md5.Sum(encrypted)

	// 4. Get CDN upload URL.
	uploadResp, err := a.callGetUploadURL(ctx, getUploadURLRequest{
		FileKey:        uuid.New().String(),
		MediaType:      3, // FILE
		ToUserID:       toUserID,
		RawSize:        len(fileData),
		RawFileMD5:     hex.EncodeToString(rawMD5[:]),
		FileSize:       len(encrypted),
		FileMD5:        hex.EncodeToString(encMD5[:]),
		ThumbRawSize:   0,
		ThumbRawFileMD5: "",
		ThumbFileSize:  0,
	})
	if err != nil {
		return nil, fmt.Errorf("get upload url: %w", err)
	}

	// 5. Extract encrypted_query_param from the upload URL for
	// use in the sendmessage payload.
	parsedURL, err := url.Parse(uploadResp.UploadFullURL)
	if err != nil {
		return nil, fmt.Errorf("parse upload url: %w", err)
	}
	encryptQueryParam := parsedURL.Query().Get("encrypted_query_param")
	if encryptQueryParam == "" {
		return nil, fmt.Errorf("upload_full_url missing encrypted_query_param: %s", uploadResp.UploadFullURL)
	}

	// 6. PUT encrypted bytes to CDN.
	if err := a.putToCDN(ctx, uploadResp.UploadFullURL, encrypted); err != nil {
		return nil, fmt.Errorf("upload to cdn: %w", err)
	}

	logger.Infof(ctx, "[WeChat] File uploaded: name=%s raw=%d enc=%d",
		fileName, len(fileData), len(encrypted))

	return &im.UploadedFileRef{
		FileName:          fileName,
		FileSize:          int64(len(fileData)),
		EncryptQueryParam: encryptQueryParam,
		AESKey:            base64.StdEncoding.EncodeToString(aesKey),
	}, nil
}

// ── getuploadurl API ──

type getUploadURLRequest struct {
	FileKey         string `json:"filekey"`
	MediaType       int    `json:"media_type"` // 1=IMAGE, 2=VIDEO, 3=FILE
	ToUserID        string `json:"to_user_id"`
	RawSize         int    `json:"rawsize"`
	RawFileMD5      string `json:"rawfilemd5"`
	FileSize        int    `json:"filesize"`
	FileMD5         string `json:"filemd5,omitempty"`
	ThumbRawSize    int    `json:"thumb_rawsize"`
	ThumbRawFileMD5 string `json:"thumb_rawfilemd5"`
	ThumbFileSize   int    `json:"thumb_filesize"`
}

type getUploadURLResponse struct {
	UploadFullURL    string `json:"upload_full_url"`
	ThumbUploadParam string `json:"thumb_upload_param,omitempty"`
}

func (a *Adapter) callGetUploadURL(ctx context.Context, req getUploadURLRequest) (*getUploadURLResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ilinkBaseURL+"/ilink/bot/getuploadurl", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	a.setAuthHeaders(httpReq, body)

	resp, err := ilinkHTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("api call: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getuploadurl returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result getUploadURLResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if result.UploadFullURL == "" {
		return nil, fmt.Errorf("empty upload_full_url in response: %s", string(respBody))
	}

	return &result, nil
}

// ── CDN HTTP PUT upload ──

func (a *Adapter) putToCDN(ctx context.Context, uploadParam string, data []byte) error {
	// upload_param is a URL or encrypted URL string returned by getuploadurl.
	// It may be a full URL or just a path fragment — try as-is first.
	uploadURL := uploadParam
	if len(uploadParam) > 0 && uploadParam[0] == '/' {
		uploadURL = cdnBaseURL + uploadParam
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create PUT request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/octet-stream")
	httpReq.Header.Set("Content-Length", fmt.Sprintf("%d", len(data)))

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("PUT cdn: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("CDN PUT returned %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}
