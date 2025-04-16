package filelu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fserrors"
)

// FileEntry represents a file entry in the JSON response
type FileEntry struct {
	Hash string `json:"hash"`
}

// APIResponse represents the response from the API.
type APIResponse struct {
	Status int `json:"status"`
	Result struct {
		Files []FileEntry `json:"files"`
	} `json:"result"`
}

// DuplicateFileError is a custom error type for duplicate files
type DuplicateFileError struct {
	Hash string
}

func (e *DuplicateFileError) Error() string {
	return "Duplicate file skipped."
}

// Helper function to handle duplicate files
//
//nolint:unused
func (f *Fs) handleDuplicate(ctx context.Context, remote string) error {
	// List files in destination
	entries, err := f.List(ctx, path.Dir(remote))
	if err != nil {
		return err
	}

	// Check if file exists
	for _, entry := range entries {
		if entry.Remote() == remote {
			// Type assert to Object
			obj, ok := entry.(fs.Object)
			if !ok {
				return fmt.Errorf("entry is not an Object")
			}
			// If file exists, remove it first
			err = obj.Remove(ctx)
			if err != nil {
				return fmt.Errorf("failed to remove existing file: %w", err)
			}
			break
		}
	}
	return nil
}

// getFileHash fetches the hash of the uploaded file using its file_code
//
//nolint:unused
func (f *Fs) getFileHash(ctx context.Context, fileCode string) (string, error) {
	apiURL := fmt.Sprintf("%s/file/info?file_code=%s&key=%s", f.endpoint, url.QueryEscape(fileCode), url.QueryEscape(f.opt.Key))

	fmt.Printf("DEBUG: Making API call to get file hash for fileCode: %s\n", fileCode)
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fserrors.FsError(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fs.Fatalf(nil, "Failed to close response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("received HTTP status %d", resp.StatusCode)
	}

	var result struct {
		Status int    `json:"status"`
		Msg    string `json:"msg"`
		Result []struct {
			Hash string `json:"hash"` // Assuming hash exists
		} `json:"result"`
	}

	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return "", fmt.Errorf("error decoding response: %w", err)
	}

	if result.Status != 200 {
		return "", fmt.Errorf("error: %s", result.Msg)
	}

	if len(result.Result) > 0 {
		if result.Result[0].Hash != "" {
			return result.Result[0].Hash, nil
		}
	}

	fmt.Println("DEBUG: Hash not found in API response.")
	return "", nil
}

// Helper method to move a single file
//
//nolint:unused
func (f *Fs) moveSingleFile(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	fs.Debugf(f, "MoveSingleFile: moving %q to %q", src.Remote(), remote)

	// Open source object for reading
	reader, err := src.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open source object: %w", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			fs.Logf(nil, "Failed to close reader: %v", err)
		}
	}()

	// Upload the file to the destination
	obj, err := f.Put(ctx, reader, src)
	if err != nil {
		return nil, fmt.Errorf("failed to move file to destination: %w", err)
	}

	// Delete the source file
	err = src.Remove(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to delete source file after move: %w", err)
	}

	fs.Debugf(f, "MoveSingleFile: successfully moved %q to %q", src.Remote(), remote)
	return obj, nil
}

// readMetaData fetches metadata for the object
//
//nolint:unused
func (o *Object) readMetaData(ctx context.Context) error {
	apiURL := fmt.Sprintf("%s/file/info?name=%s&key=%s",
		o.fs.endpoint, url.QueryEscape(o.remote), url.QueryEscape(o.fs.opt.Key))

	var result struct {
		Status  int    `json:"status"`
		Msg     string `json:"msg"`
		Size    int64  `json:"size"`
		ModTime string `json:"mod_time"`
	}

	err := o.fs.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := o.fs.client.Do(req)
		if err != nil {
			return shouldRetry(err), err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return false, fs.ErrorObjectNotFound
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return false, err
		}

		return shouldRetryHTTP(resp.StatusCode), nil
	})
	if err != nil || result.Status != 200 {
		return fs.ErrorObjectNotFound
	}

	o.size = result.Size
	o.modTime, err = time.Parse(time.RFC3339, result.ModTime)
	if err != nil {
		o.modTime = time.Now()
	}
	return nil
}

// fetchRemoteFileHashes retrieves hashes of remote files in a folder
//
//nolint:unused
func (f *Fs) fetchRemoteFileHashes(ctx context.Context, folderID int) (map[string]struct{}, error) {
	apiURL := fmt.Sprintf("%s/folder/list?fld_id=%d&key=%s", f.endpoint, folderID, url.QueryEscape(f.opt.Key))
	var debugResp []byte
	var result struct {
		Status int `json:"status"`
		Result struct {
			Files []struct {
				Hash string `json:"hash"`
			} `json:"files"`
		} `json:"result"`
	}

	err := f.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := f.client.Do(req)
		if err != nil {
			return shouldRetry(err), err
		}
		defer resp.Body.Close()

		debugResp, err = io.ReadAll(resp.Body)
		return shouldRetryHTTP(resp.StatusCode), err
	})
	if err != nil {
		return nil, err
	}

	fs.Debugf(f, "Raw API Response: %s", string(debugResp))

	if err := json.Unmarshal(debugResp, &result); err != nil {
		return nil, err
	}
	if result.Status != 200 {
		return nil, fmt.Errorf("error: non-200 status %d", result.Status)
	}

	hashes := make(map[string]struct{})
	for _, file := range result.Result.Files {
		fs.Debugf(f, "Fetched remote hash: %s", file.Hash)
		hashes[file.Hash] = struct{}{}
	}
	fs.Debugf(f, "Total fetched remote hashes: %d", len(hashes))
	return hashes, nil
}

// IsDuplicateFileError checks if the given error indicates a duplicate file.
func IsDuplicateFileError(err error) bool {
	_, ok := err.(*DuplicateFileError)
	return ok
}

// listFolderRaw to list folder with its full path.
func (f *Fs) listFolderRaw(ctx context.Context, folderPath string) (*FolderListResponse, error) {
	apiURL := fmt.Sprintf("%s/folder/list?folder_path=%s&&key=%s",
		f.endpoint,
		url.QueryEscape(folderPath),
		url.QueryEscape(f.opt.Key),
	)

	var body []byte
	err := f.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := f.client.Do(req)
		if err != nil {
			return shouldRetry(err), err
		}
		defer resp.Body.Close()
		body, err = io.ReadAll(resp.Body)
		return shouldRetryHTTP(resp.StatusCode), err
	})
	if err != nil {
		return nil, err
	}

	var result FolderListResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&result); err != nil {
		return nil, err
	}

	return &result, nil
}

// getFileSize retrieves the size of a file
func (f *Fs) getFileSize(ctx context.Context, filePath string) (int64, error) {
	filePath = "/" + strings.Trim(filePath, "/")

	apiURL := fmt.Sprintf("%s/file/info?file_path=%s&key=%s",
		f.endpoint,
		url.QueryEscape(filePath),
		url.QueryEscape(f.opt.Key),
	)

	fs.Debugf(f, "getFileSize: Fetching file info from %s", apiURL)

	var sizeStr string
	err := f.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := f.client.Do(req)
		if err != nil {
			return shouldRetry(err), fmt.Errorf("failed to fetch file info: %w", err)
		}
		defer resp.Body.Close()

		var result struct {
			Status int    `json:"status"`
			Msg    string `json:"msg"`
			Result []struct {
				Size string `json:"size"`
			} `json:"result"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return false, fmt.Errorf("error decoding response: %w", err)
		}

		if result.Status != 200 || len(result.Result) == 0 {
			return false, fmt.Errorf("error fetching file info: %s", result.Msg)
		}

		sizeStr = result.Result[0].Size
		return shouldRetryHTTP(resp.StatusCode), nil
	})
	if err != nil {
		return 0, err
	}

	return convertSizeStringToInt64(sizeStr), nil
}

// getDirectLinkUsingFileCode of file from FileLu to download.
func (f *Fs) getDirectLinkUsingFileCode(ctx context.Context, fileCode string) (string, int64, error) {
	apiURL := fmt.Sprintf("%s/file/direct_link?file_code=%s&key=%s",
		f.endpoint,
		url.QueryEscape(fileCode),
		url.QueryEscape(f.opt.Key),
	)

	fs.Debugf(f, "getDirectLink: fetching direct link for file code %q", fileCode)

	result := FileDirectLinkResponse{}
	err := f.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create request: %w", err)
		}

		resp, err := f.client.Do(req)
		if err != nil {
			return shouldRetry(err), fmt.Errorf("failed to fetch direct link: %w", err)
		}
		defer resp.Body.Close()

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return false, fmt.Errorf("error decoding response: %w", err)
		}

		if result.Status != 200 {
			return false, fmt.Errorf("API error: %s", result.Msg)
		}

		return shouldRetryHTTP(resp.StatusCode), nil
	})
	if err != nil {
		return "", 0, err
	}

	fs.Debugf(f, "getDirectLink: obtained URL %q with size %d", result.Result.URL, result.Result.Size)
	return result.Result.URL, result.Result.Size, nil
}

// moveFileToFolder moves a file to a different folder using file paths
func (f *Fs) moveFileToFolder(ctx context.Context, filePath string, destinationPath string) error {
	filePath = "/" + strings.Trim(filePath, "/")
	destinationPath = "/" + strings.Trim(destinationPath, "/")
	filePath = f.FromStandardPath(filePath)
	destinationPath = f.FromStandardPath(destinationPath)

	apiURL := fmt.Sprintf("%s/file/set_folder?file_path=%s&destination_folder_path=%s&key=%s",
		f.endpoint,
		url.QueryEscape(filePath),
		url.QueryEscape(destinationPath),
		url.QueryEscape(f.opt.Key),
	)

	fs.Debugf(f, "moveFileToFolder: Sending move request to %s", apiURL)

	var result struct {
		Status int    `json:"status"`
		Msg    string `json:"msg"`
	}

	err := f.pacer.Call(func() (bool, error) {
		req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
		if err != nil {
			return false, fmt.Errorf("failed to create move request: %w", err)
		}

		resp, err := f.client.Do(req)
		if err != nil {
			return shouldRetry(err), fmt.Errorf("failed to send move request: %w", err)
		}
		defer resp.Body.Close()

		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			return false, fmt.Errorf("error decoding move response: %w", err)
		}

		if result.Status != 200 {
			return false, fmt.Errorf("error while moving file: %s", result.Msg)
		}

		return shouldRetryHTTP(resp.StatusCode), nil
	})

	if err != nil {
		return err
	}

	fs.Debugf(f, "moveFileToFolder: Successfully moved file %q to folder %q", filePath, destinationPath)
	return nil
}

// createTempFileFromReader writes the content of the 'in' reader into a temporary file
func createTempFileFromReader(in io.Reader) (string, error) {
	// Create a temporary file
	tempFile, err := os.CreateTemp("", "upload-*.tmp")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}

	// Defer the closing of the temp file to ensure it gets closed after copying
	defer func() {
		err = tempFile.Close()
		if err != nil {
			fs.Logf(nil, "Failed to close temporary file: %v", err)
		}
	}()

	// Copy the data to the temp file
	_, err = io.Copy(tempFile, in)
	if err != nil {
		// Attempt to remove the file if copy operation fails
		defer func() {
			if err := os.Remove(tempFile.Name()); err != nil {
				fs.Logf(nil, "Failed to remove temp file %q: %v", tempFile.Name(), err)
			}
		}()

		return "", fmt.Errorf("failed to copy data to temp file: %w", err)
	}

	return tempFile.Name(), nil
}
