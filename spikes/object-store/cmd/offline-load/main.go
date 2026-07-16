package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"
)

const (
	dockerSocket     = "/var/run/docker.sock"
	maxVersionBytes  = 64 * 1024
	maxResponseBytes = 1024 * 1024
)

var apiVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

func main() {
	if len(os.Args) != 2 {
		fatal()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := loadArchive(ctx, dockerSocket, os.Args[1]); err != nil {
		fatal()
	}
}

func loadArchive(ctx context.Context, socketPath, archivePath string) error {
	archive, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 {
		return errors.New("invalid image archive")
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Minute}

	versionRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return err
	}
	versionResponse, err := client.Do(versionRequest)
	if err != nil {
		return err
	}
	versionData, readErr := readBounded(versionResponse.Body, maxVersionBytes)
	versionResponse.Body.Close()
	if readErr != nil || versionResponse.StatusCode != http.StatusOK {
		return errors.New("Docker version request failed")
	}
	var version struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.Unmarshal(versionData, &version); err != nil || !apiVersionPattern.MatchString(version.APIVersion) {
		return errors.New("Docker API version was invalid")
	}

	endpoint := fmt.Sprintf("http://docker/v%s/images/load?quiet=1", version.APIVersion)
	loadRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, archive)
	if err != nil {
		return err
	}
	loadRequest.Header.Set("Content-Type", "application/x-tar")
	loadRequest.ContentLength = info.Size()
	loadResponse, err := client.Do(loadRequest)
	if err != nil {
		return err
	}
	responseData, readErr := readBounded(loadResponse.Body, maxResponseBytes)
	loadResponse.Body.Close()
	if readErr != nil || loadResponse.StatusCode < 200 || loadResponse.StatusCode >= 300 {
		return errors.New("Docker image load request failed")
	}
	return validateLoadResponse(responseData)
}

func validateLoadResponse(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	messages := 0
	for {
		var message struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		err := decoder.Decode(&message)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || message.Error != "" || message.ErrorDetail.Message != "" {
			return errors.New("Docker image load stream reported an error")
		}
		messages++
	}
	if messages == 0 {
		return errors.New("Docker image load stream was empty")
	}
	return nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("Docker response exceeded bound")
	}
	return data, nil
}

func fatal() {
	fmt.Fprintln(os.Stderr, "network-isolated local image import failed")
	os.Exit(1)
}
