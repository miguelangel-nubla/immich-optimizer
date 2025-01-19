package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"text/template"

	"github.com/spf13/viper"
)

var remote *url.URL
var convertCMDTemplate *template.Template
var (
	currentJobID          = 0
	maxConcurrentRequests = 10
	semaphore             = make(chan struct{}, maxConcurrentRequests)
)

var upstreamURL string
var listenAddr string
var convertCMD string
var filterPath string
var filterFormKey string

func init() {
	viper.SetEnvPrefix("iuo")
	viper.AutomaticEnv()
	viper.BindEnv("upstream")
	viper.BindEnv("listen")
	viper.BindEnv("convert_cmd")
	viper.BindEnv("filter_path")
	viper.BindEnv("filter_form_key")

	viper.SetDefault("upstream", "")
	viper.SetDefault("listen", ":2283")
	viper.SetDefault("convert_cmd", "caesiumclt --keep-dates --exif --quality=0 --output={{.dirname}} {{.filename}}")
	viper.SetDefault("filter_path", "/api/assets")
	viper.SetDefault("filter_form_key", "assetData")

	flag.StringVar(&upstreamURL, "upstream", viper.GetString("upstream"), "Upstream URL. Example: http://immich-server:2283")
	flag.StringVar(&listenAddr, "listen", viper.GetString("listen"), "Listening address")
	flag.StringVar(&convertCMD, "convert_cmd",
		viper.GetString("convert_cmd"),
		"Command to apply to convert image, available placeholders: dirname, filename. "+
			"This utility will read the converted file from the same filename, so you need to overwrite the original. "+
			"The file is in a temp folder by itself.")
	flag.StringVar(&filterPath, "filter-path", viper.GetString("filter_path"), "Only convert images uploaded to specific path. Advanced, leave default for immich")
	flag.StringVar(&filterFormKey, "filter-form-key", viper.GetString("filter_form_key"), "Only convert images uploaded with specific form key. Advanced, leave default for immich")
	flag.Parse()
	validateInput()
}

func validateInput() {
	if upstreamURL == "" {
		log.Fatal("the -upstream flag is required")
	}

	var err error
	remote, err = url.Parse(upstreamURL)
	if err != nil {
		log.Fatalf("invalid upstream URL: %v", err)
	}

	convertCMDTemplate, err = template.New("command").Parse(convertCMD)
	if err != nil {
		log.Fatalf("invalid convert command: %v", err)
	}

	values := map[string]string{
		"dirname":  "/test",
		"filename": "/test/file.name",
	}
	var cmdLine bytes.Buffer
	err = convertCMDTemplate.Execute(&cmdLine, values)
	if err != nil {
		log.Fatalf("invalid convert command: %v", err)
	}
}

func processImage(file io.Reader) ([]byte, error) {
	tempDir, err := os.MkdirTemp("", "image-processing-*")
	if err != nil {
		return nil, fmt.Errorf("unable to create temp folder: %w", err)
	}
	defer os.RemoveAll(tempDir)

	tempFile, err := os.CreateTemp(tempDir, "input-*.jpg")
	if err != nil {
		return nil, fmt.Errorf("unable to create temp file: %w", err)
	}

	_, err = io.Copy(tempFile, file)
	if err != nil {
		return nil, fmt.Errorf("unable to write temp file: %w", err)
	}
	tempFile.Close()

	values := map[string]string{
		"dirname":  tempDir,
		"filename": tempFile.Name(),
	}
	var cmdLine bytes.Buffer
	err = convertCMDTemplate.Execute(&cmdLine, values)
	if err != nil {
		return nil, fmt.Errorf("unable to generate convert command: %w", err)
	}

	cmd := exec.Command("sh", "-c", cmdLine.String())
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("unable to run convert command \"%s\", error: %w", cmdLine.String(), err)
	}

	tempFile, err = os.Open(tempFile.Name())
	if err != nil {
		return nil, fmt.Errorf("unable to open temp file: %w", err)
	}
	defer tempFile.Close()

	processedImage, err := io.ReadAll(tempFile)
	if err != nil {
		return nil, fmt.Errorf("unable to read temp file: %w", err)
	}

	return processedImage, nil
}

func handleMultipartUpload(w http.ResponseWriter, r *http.Request, formFileKey string) (filename string, originalSize int64, newSize int64, replace bool, err error) {
	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	replace = false
	err = r.ParseMultipartForm(100 << 30) // 100 MB max memory
	if err != nil {
		err = fmt.Errorf("unable to parse multipart form: %w", err)
		return
	}

	originalImage, handler, err := r.FormFile(formFileKey)
	if err != nil {
		err = fmt.Errorf("unable to read form file key %s in uploaded form data: %w", formFileKey, err)
		return
	}
	defer originalImage.Close()

	originalSize = handler.Size
	filename = handler.Filename

	processedImage, err := processImage(originalImage)
	if err != nil {
		err = fmt.Errorf("unable to process image: %w", err)
		return
	}

	newSize = int64(len(processedImage))

	replace = originalSize > newSize

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	for key, values := range r.MultipartForm.Value {
		for _, value := range values {
			err = writer.WriteField(key, value)
			if err != nil {
				err = fmt.Errorf("unable to create form data to be sent upstream: %w", err)
				return
			}
		}
	}

	part, err := writer.CreateFormFile(formFileKey, handler.Filename)
	if err != nil {
		err = fmt.Errorf("unable to create image form field to be sent upstream: %w", err)
		return
	}

	if replace {
		_, err = part.Write(processedImage)
	} else {
		_, err = io.Copy(part, originalImage)
	}
	if err != nil {
		err = fmt.Errorf("unable to write image in form field to be sent upstream: %w", err)
		return
	}

	err = writer.Close()
	if err != nil {
		err = fmt.Errorf("unable to finish form data to be sent upstream: %w", err)
		return
	}

	destination := *remote
	destination.Path = path.Join(destination.Path, r.URL.Path)
	req, err := http.NewRequest("POST", destination.String(), &buffer)
	if err != nil {
		err = fmt.Errorf("unable to create POST request to upstream: %w", err)
		return
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("unable to POST to upstream: %w", err)
		return
	}
	defer resp.Body.Close()

	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		err = fmt.Errorf("unable forward response back to client from upstream: %w", err)
		return
	}

	return
}

func main() {
	proxy := httputil.NewSingleHostReverseProxy(remote)

	handler := func(w http.ResponseWriter, r *http.Request) {
		match, err := path.Match(filterPath, r.URL.Path)
		if err != nil {
			log.Printf("invalid filter-path: %s", r.URL)
			return
		}
		if !match || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			log.Printf("proxy request: %s", r.URL)

			r.Host = remote.Host
			proxy.ServeHTTP(w, r)

			return
		}

		currentJobID++
		jobID := currentJobID
		log.Printf("job %d: incoming image upload on \"%s\" from %s, intercepting...", jobID, r.URL, r.RemoteAddr)
		filename, originalSize, newSize, replaced, err := handleMultipartUpload(w, r, filterFormKey)
		if err != nil {
			log.Printf("job %d: Failed to process upload: %v", jobID, err.Error())
			http.Error(w, "failed to process upload, view logs for more info", http.StatusInternalServerError)
			return
		}

		action := "image NOT replaced"
		if replaced {
			action = "image replaced"
		}

		log.Printf("job %d: %s: \"%s\" original = %s, optimized = %s", jobID, action, filename, humanReadableSize(originalSize), humanReadableSize(newSize))
	}

	http.HandleFunc("/", handler)

	log.Printf("Starting immich-upload-optimizer on %s...", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("Error starting immich-upload-optimizer: %v", err)
	}
}

func humanReadableSize(size int64) string {
	const (
		_  = iota // ignore first value by assigning to blank identifier
		KB = 1 << (10 * iota)
		MB
		GB
		TB
	)

	switch {
	case size >= TB:
		return fmt.Sprintf("%.2f TB", float64(size)/TB)
	case size >= GB:
		return fmt.Sprintf("%.2f GB", float64(size)/GB)
	case size >= MB:
		return fmt.Sprintf("%.2f MB", float64(size)/MB)
	case size >= KB:
		return fmt.Sprintf("%.2f KB", float64(size)/KB)
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}
