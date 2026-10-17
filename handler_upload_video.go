package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	videoID, err := uuid.Parse(r.PathValue("videoID"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get video metadata", err)
		return
	}

	if userID != videoMetadata.UserID {
		respondWithError(w, http.StatusUnauthorized, "User doesn't own this video", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	contentType := header.Header.Get("Content-Type")
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to process media type", err)
		return
	}

	if mediatype != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, fmt.Sprintf("Incorrect mediatype. Expected video/mp4, got %s", mediatype), nil)
		return
	}

	outputTempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temp file", err)
		return
	}
	defer os.Remove(outputTempFile.Name())
	defer outputTempFile.Close()

	_, err = io.Copy(outputTempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to write file data", err)
		return
	}

	_, err = outputTempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to seek the start of the file", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(outputTempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get the video's ffprobe metadata", err)
		return
	}

	switch aspectRatio {
	case "16:9":
		aspectRatio = "landscape"
		break
	case "9:16":
		aspectRatio = "portrait"
		break
	default:
		aspectRatio = "other"
	}

	rnd := make([]byte, 32)
	_, err = rand.Read(rnd)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to generate 32-byte random", err)
		return
	}
	key := fmt.Sprintf("%s/%x.mp4", aspectRatio, rnd)

	processedFilePath, err := processVideoForFastStart(outputTempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video file", err)
		return
	}
	defer os.Remove(processedFilePath)

	processedFileBody, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not open processed video file", err)
		return
	}
	defer processedFileBody.Close()

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &key,
		Body:        processedFileBody,
		ContentType: &mediatype,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to put object on s3", err)
		return
	}

	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, key)

	videoMetadata.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to update video metadata", err)
		return
	}

	videoMetadata, err = cfg.dbVideoToSignedVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to get presigned video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMetadata)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)

	var buffer bytes.Buffer
	cmd.Stdout = &buffer

	err := cmd.Run()
	if err != nil {
		return "", err
	}

	type ffprobeStreams struct {
		Streams []struct {
			Index              int    `json:"index"`
			CodecName          string `json:"codec_name,omitempty"`
			Profile            string `json:"profile,omitempty"`
			CodecType          string `json:"codec_type"`
			CodecTagString     string `json:"codec_tag_string"`
			Width              int    `json:"width,omitempty"`
			Height             int    `json:"height,omitempty"`
			DisplayAspectRatio string `json:"display_aspect_ratio,omitempty"`
		} `json:"streams"`
	}

	var streams ffprobeStreams
	if err := json.Unmarshal(buffer.Bytes(), &streams); err != nil {
		return "", err
	}

	switch streams.Streams[0].DisplayAspectRatio {
	case "16:9", "9:16":
		return streams.Streams[0].DisplayAspectRatio, nil
	default:
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := fmt.Sprintf("%s.processing", filePath)

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("error processing video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(outputPath)
	if err != nil {
		return "", fmt.Errorf("could not stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return outputPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	r, err := presignClient.PresignGetObject(context.Background(), &s3.GetObjectInput{Bucket: &bucket, Key: &key}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}

	return r.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	bucketKey := strings.Split(*video.VideoURL, ",")
	if len(bucketKey) != 2 {
		return video, fmt.Errorf("invalid video url: %s", *video.VideoURL)
	}

	newUrl, err := generatePresignedURL(cfg.s3Client, bucketKey[0], bucketKey[1], (time.Minute * 5))
	if err != nil {
		return video, err
	}

	video.VideoURL = &newUrl
	return video, nil
}
