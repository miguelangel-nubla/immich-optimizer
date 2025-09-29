package main

// uploadToImmich uploads a file to the Immich server
func (fw *FileWatcher) uploadToImmich(uploadFilePath string) {
	err := fw.immichClient.UploadAsset(uploadFilePath)
	if err != nil {
		fw.handleUploadError(uploadFilePath, err)
	}
}

// handleUploadError handles errors that occur during file upload
func (fw *FileWatcher) handleUploadError(filePath string, err error) {
	fw.logger.Printf("Error uploading file %s to Immich: %v", filePath, err)
	if copyErr := copyFileToUndone(filePath, fw.watchDir, fw.appConfig.UndoneDir); copyErr != nil {
		fw.logger.Printf("Error copying file %s to undone directory: %v", filePath, copyErr)
	}
}