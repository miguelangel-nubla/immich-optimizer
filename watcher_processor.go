package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// processFile handles the complete file processing workflow
func (fw *FileWatcher) processFile(originalFilePath string) {
	if !fw.validateFile(originalFilePath) {
		return
	}

	fw.logger.Printf("Processing file: %s", originalFilePath)

	if !fw.shouldOptimizeFile(originalFilePath) {
		fw.uploadToImmich(originalFilePath)
		return
	}

	tp, err := fw.createTaskProcessor(originalFilePath)
	if err != nil {
		fw.logger.Printf("Error creating task processor for %s: %v", originalFilePath, err)
		return
	}
	defer tp.Close()

	if err := tp.Process(fw.config.Tasks); err != nil {
		fw.handleProcessingError(originalFilePath, err)
		return
	}

	fw.handleProcessingSuccess(originalFilePath, tp)
	fw.cleanupOriginalFile(originalFilePath)
}

// validateFile checks if the file exists and is not a directory
func (fw *FileWatcher) validateFile(filePath string) bool {
	info, err := os.Stat(filePath)
	if err != nil {
		fw.logger.Printf("Error getting file info for %s: %v", filePath, err)
		return false
	}
	return !info.IsDir()
}

// shouldOptimizeFile determines if a file should be processed for optimization
func (fw *FileWatcher) shouldOptimizeFile(filePath string) bool {
	extension := filepath.Ext(filePath)
	if !shouldProcessExtension(extension, fw.config.Tasks) {
		fw.logger.Printf("Skipping file %s (extension %s not configured for processing)", filePath, extension)
		return false
	}
	return true
}

// createTaskProcessor creates and configures a new task processor for the file
func (fw *FileWatcher) createTaskProcessor(filePath string) (*TaskProcessor, error) {
	tp, err := NewTaskProcessor(filePath)
	if err != nil {
		return nil, err
	}

	jobLogger := newCustomLogger(fw.logger, fmt.Sprintf("file %s: ", filePath))
	tp.SetLogger(jobLogger)

	if fw.appConfig != nil {
		tp.SetSemaphore(fw.appConfig.Semaphore)
		tp.SetConfigDir(filepath.Dir(fw.appConfig.ConfigFile))
	}

	return tp, nil
}

// handleProcessingError handles errors that occur during file processing
func (fw *FileWatcher) handleProcessingError(filePath string, err error) {
	fw.logger.Printf("Error processing file %s: %v", filePath, err)
	if copyErr := copyFileToUndone(filePath, fw.watchDir, fw.appConfig.UndoneDir); copyErr != nil {
		fw.logger.Printf("Error copying file %s to undone directory: %v", filePath, copyErr)
	}
}

// handleProcessingSuccess handles successful file processing and determines upload strategy
func (fw *FileWatcher) handleProcessingSuccess(originalFilePath string, tp *TaskProcessor) {
	if fw.shouldUploadProcessedFile(tp) {
		fw.uploadProcessedFile(originalFilePath, tp)
	} else {
		fw.uploadOriginalFile(originalFilePath)
	}
}

// shouldUploadProcessedFile determines if the processed file should be uploaded instead of original
func (fw *FileWatcher) shouldUploadProcessedFile(tp *TaskProcessor) bool {
	return tp.ProcessedFile != nil && tp.ProcessedSize > 0 && tp.OriginalSize > tp.ProcessedSize
}

// uploadProcessedFile uploads the optimized version of the file
func (fw *FileWatcher) uploadProcessedFile(originalFilePath string, tp *TaskProcessor) {
	processedFilePath, err := tp.GetProcessedFilePath()
	if err != nil {
		fw.logger.Printf("Error getting processed file path: %v", err)
		fw.uploadToImmich(originalFilePath)
		return
	}

	fw.logger.Printf("Optimized file uploaded: %s -> %s",
		humanReadableSize(tp.OriginalSize),
		humanReadableSize(tp.ProcessedSize))
	fw.uploadToImmich(processedFilePath)
}

// uploadOriginalFile uploads the original file without optimization
func (fw *FileWatcher) uploadOriginalFile(filePath string) {
	fw.logger.Printf("Original file uploaded (no optimization achieved)")
	fw.uploadToImmich(filePath)
}

// cleanupOriginalFile removes the original file after successful processing
func (fw *FileWatcher) cleanupOriginalFile(filePath string) {
	if err := os.Remove(filePath); err != nil {
		fw.logger.Printf("Error removing file %s after upload: %v", filePath, err)
	}
}
