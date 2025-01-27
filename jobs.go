package main

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

func newJob(r *http.Request, w http.ResponseWriter, logger *customLogger) (err error) {
	jobID := uuid.New().String()

	// Check if client has broken redirect behavior, like the android app.
	// Redirect support is necessary as file processing can take a long time and the client risks a timeout on the http request.
	// Currently no way to check for redirect support so blacklist Dart/ user agent from the android app.
	// Ideally redirect support is added here: https://github.com/immich-app/immich/blob/f6cbc9db06c0783d09f154f66e12d041032fff62/cli/src/commands/asset.ts#L290
	clientFollowsRedirects := !strings.HasPrefix(r.UserAgent(), "Dart/")
	if !clientFollowsRedirects {
		logger = newCustomLogger(logger, "client with broken redirects: ")
	}

	jobLogger := newCustomLogger(logger, fmt.Sprintf("job %s: ", jobID))

	jobLogger.Printf("intercepting upload")

	// Parse the form data
	err = r.ParseMultipartForm(0) // always write to file
	if err != nil {
		err = fmt.Errorf("unable to parse multipart form: %w", err)
		return
	}
	originalFile, handler, err := r.FormFile(filterFormKey)
	if err != nil {
		err = fmt.Errorf("unable to read file in key %s from uploaded form data: %w", filterFormKey, err)
		return
	}
	defer originalFile.Close()

	// Create the channels for this job
	jobChannels[jobID] = make(chan *http.Response)
	defer close(jobChannels[jobID])
	defer delete(jobChannels, jobID)
	jobChannelsComplete[jobID] = make(chan struct{})
	defer close(jobChannelsComplete[jobID])
	defer delete(jobChannelsComplete, jobID)

	// Redirect the user to the job wait page
	if clientFollowsRedirects {
		http.Redirect(w, r, fmt.Sprintf("/_immich-upload-optimizer/wait?job=%s", jobID), http.StatusTemporaryRedirect)
		w.(http.Flusher).Flush()
	}

	// Continue processing the file
	originalSize := handler.Size
	originalFilename := handler.Filename
	basename := path.Base(originalFilename)
	extension := path.Ext(basename)

	jobLogger.Printf("uploaded %s %s", originalFilename, humanReadableSize(originalSize))

	processedFile, newExtension, newSize, err := processTasks(originalFile, extension, jobLogger)
	if err != nil {
		jobLogger.Printf("failed to process file: %v", err.Error())
		if !clientFollowsRedirects {
			http.Error(w, "failed to process file, view logs for more info", http.StatusInternalServerError)
		}
		return
	}

	newFilename := strings.TrimSuffix(originalFilename, extension) + newExtension
	replaced := originalSize > newSize

	// Create the form data to be sent upstream
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

	uploadFilename := originalFilename
	if replaced {
		uploadFilename = newFilename
	}
	part, err := writer.CreateFormFile(filterFormKey, uploadFilename)
	if err != nil {
		err = fmt.Errorf("unable to create file form field to be sent upstream: %w", err)
		return
	}

	if replaced {
		_, err = io.Copy(part, processedFile)
	} else {
		// multipart.File is io.ReaderAt, so we can't just copy it assuming it is at the beginning of the file.
		_, err = io.Copy(part, io.NewSectionReader(originalFile, 0, originalSize))
	}
	if err != nil {
		err = fmt.Errorf("unable to write file in form field to be sent upstream: %w", err)
		return
	}

	err = writer.Close()
	if err != nil {
		err = fmt.Errorf("unable to finish form data to be sent upstream: %w", err)
		return
	}

	// Send the request to the upstream server
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

	// Log the result
	action := "file NOT replaced"
	if replaced {
		action = "file replaced"
	}

	jobLogger.Printf("%s: \"%s\" %s optimized to \"%s\" %s", action, originalFilename, humanReadableSize(originalSize), newFilename, humanReadableSize(newSize))

	if !clientFollowsRedirects {
		w.WriteHeader(resp.StatusCode)
		_, err := io.Copy(w, resp.Body)
		if err != nil {
			jobLogger.Printf("unable to forward response back to client directly: %v", err)
		} else {
			jobLogger.Printf("response sent back to client directly")
		}
		return nil
	}

	// Send the response back to the client via the wait page
	select {
	case jobChannels[jobID] <- resp:
		// Wait for the response to be sent to the client before cleaning up or timeout.
		// This is to avoid all the deferred functions to run before the response is fully sent.
		select {
		case <-jobChannelsComplete[jobID]:
			jobLogger.Printf("response sent to client")
		case <-time.After(10 * time.Second):
			jobLogger.Printf("timeout before response was fully sent to client")
		}
	case <-time.After(10 * time.Second):
		jobLogger.Printf("timeout while waiting for client to ask for a response on the redirect wait page, redirect was not followed by the client.")
	}

	return nil
}

func continueJob(r *http.Request, w http.ResponseWriter, requestLogger *customLogger) {
	jobID := r.URL.Query().Get("job")
	jobChannel, exists := jobChannels[jobID]
	if jobID == "" || !exists {
		http.Error(w, "job not found", http.StatusBadRequest)
		return
	}

	jobLogger := newCustomLogger(requestLogger, fmt.Sprintf("job %s: ", jobID))

	// 55s to avoid browser timeout
	safeClientTimeout := time.Duration(55) * time.Second

	select {
	case resp, ok := <-jobChannel:
		if !ok {
			msg := "job channel closed unexpectedly"
			http.Error(w, msg, http.StatusInternalServerError)
			requestLogger.Printf(msg)
			return
		}
		w.WriteHeader(resp.StatusCode)
		_, err := io.Copy(w, resp.Body)
		if err != nil {
			jobLogger.Printf("unable to forward response back to client: %v", err)
		}
		jobChannelsComplete[jobID] <- struct{}{}
	case <-time.After(safeClientTimeout):
		http.Redirect(w, r, r.URL.String(), http.StatusTemporaryRedirect)
		jobLogger.Printf("still running, sending redirect to avoid client timeout: %s", r.URL)
	}
}
