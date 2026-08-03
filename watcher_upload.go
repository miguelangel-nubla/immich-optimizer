package main

import "path/filepath"

// uploadToImmich uploads a file to the Immich server, preserving the file's current name
func (fw *FileWatcher) uploadToImmich(uploadFilePath string) error {
	return fw.uploadToImmichWithFilename(uploadFilePath, filepath.Base(uploadFilePath))
}

// uploadToImmichWithFilename uploads a file using the provided filename metadata
func (fw *FileWatcher) uploadToImmichWithFilename(uploadFilePath, filename string) error {
	return fw.immichClient.UploadAssetWithFilename(uploadFilePath, filename)
}

// handleUploadError handles errors that occur during file upload
func (fw *FileWatcher) handleUploadError(filePath string, err error) {
	fw.logger.Printf("Error uploading file %s to Immich: %v", filePath, err)
	if moveErr := moveToUndone(filePath, fw.watchDir, fw.appConfig.UndoneDir); moveErr != nil {
		fw.logger.Printf("Error moving file %s to undone directory: %v", filePath, moveErr)
	}
}
