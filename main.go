package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/firestore"
	"cloud.google.com/go/storage"
	"github.com/jung-kurt/gofpdf"
	"github.com/line/line-bot-sdk-go/linebot"
	"google.golang.org/api/option"
)

type Image struct {
	UserID    string
	ImageURL  string
	FileName  string
	CreatedAt time.Time
}

type UserState struct {
	state     string
	imageData *Image
}

var userStates = make(map[string]*UserState)

func isFileNameValid(fileName string) bool {
	trimmed := strings.TrimSpace(fileName)
	if trimmed == "" {
		return false
	}
	if strings.Contains(trimmed, "pdf") {
		return false
	}
	return true
}

func createFirestoreClient(ctx context.Context) *firestore.Client {
	saPath := "path/to/your-service-account-key.json"

	client, err := firestore.NewClient(ctx, "your-project-id", option.WithCredentialsFile(saPath))
	if err != nil {
		log.Fatalf("Failed to create Firestore client: %v", err)
	}

	return client
}

func SaveImageToFirestore(ctx context.Context, client *firestore.Client, img *Image) {
	docRef := client.Collection("images").Doc(img.UserID)
	_, err := docRef.Set(ctx, img)
	if err != nil {
		log.Printf("Failed to save image to Firestore: %v", err)
	}
}

func createLineBotClient() *linebot.Client {
	channelSecret := "your_channel_secret"
	channelAccessToken := "your_channel_access_token"

	bot, err := linebot.New(channelSecret, channelAccessToken)
	if err != nil {
		log.Fatalf("Failed to create LINE bot client: %v", err)
	}

	return bot
}

func uploadToCloudStorage(ctx context.Context, bucketName, objectName, localFile string) error {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	f, err := os.Open(localFile)
	if err != nil {
		return err
	}
	defer f.Close()

	wc := client.Bucket(bucketName).Object(objectName).NewWriter(ctx)
	if _, err = io.Copy(wc, f); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}

	return nil
}

func convertImageToPDF(imagePath, pdfPath string) error {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.AddPage()

	imageOptions := gofpdf.ImageOptions{
		ImageType: "JPG",
	}

	width, _ := pdf.GetPageSize()
	pdf.ImageOptions(imagePath, 0, 0, width, 0, false, imageOptions, 0, "")

	return pdf.OutputFileAndClose(pdfPath)
}

func main() {
	ctx := context.Background()
	firestoreClient := createFirestoreClient(ctx)
	lineBotClient := createLineBotClient()

	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		events, err := lineBotClient.ParseRequest(r)
		if err != nil {
			log.Printf("Failed to parse request: %v", err)
			return
		}

		for _, event := range events {
			if event.Type == linebot.EventTypeMessage {
				userID := event.Source.UserID
				state := userStates[userID]

				switch message := event.Message.(type) {
				case *linebot.ImageMessage:
					if state != nil && state.state == "waiting_for_filename" {
						continue
					}
					imageURL := message.OriginalContentURL

					img := &Image{
						UserID:    userID,
						ImageURL:  imageURL,
						FileName:  "", // Set later when the user provides the filename
						CreatedAt: time.Now(),
					}

					userStates[userID] = &UserState{
						state:     "waiting_for_filename",
						imageData: img,
					}

					_, err = lineBotClient.ReplyMessage(event.ReplyToken, linebot.NewTextMessage("画像を受け取りました。ファイル名を入力してください。")).Do()
					if err != nil {
						log.Printf("Failed to reply message: %v", err)
					}
				case *linebot.TextMessage:
					if state != nil && state.state == "waiting_for_filename" {
						fileName := message.Text

						if !isFileNameValid(fileName) {
							_, err = lineBotClient.ReplyMessage(event.ReplyToken, linebot.NewTextMessage("無効なファイル名です。もう一度入力してください。")).Do()
							if err != nil {
								log.Printf("Failed to reply message: %v", err)
							}
							continue

						}

						// Set the filename for the image
						state.imageData.FileName = fileName

						// Convert image to PDF
						pdfPath := userID + ".pdf"
						err = convertImageToPDF(state.imageData.ImageURL, pdfPath)
						if err != nil {
							log.Printf("Failed to convert image to PDF: %v", err)
							continue
						}

						// Upload PDF to Google Cloud Storage
						bucketName := "your-bucket-name"
						objectName := userID + "/" + pdfPath
						err = uploadToCloudStorage(ctx, bucketName, objectName, pdfPath)
						if err != nil {
							log.Printf("Failed to upload PDF to Cloud Storage: %v", err)
							continue
						}

						// Generate download URL for the uploaded PDF
						downloadURL := "https://storage.googleapis.com/" + bucketName + "/" + objectName

						// Save the image data to Firestore
						SaveImageToFirestore(ctx, firestoreClient, state.imageData)

						// Reset the user state
						userStates[userID] = nil

						_, err = lineBotClient.ReplyMessage(event.ReplyToken, linebot.NewTextMessage("PDFに変換しました。"+downloadURL)).Do()
						if err != nil {
							log.Printf("Failed to reply message: %v", err)
						}
					}
				}
			}
		}
	})

	log.Fatal(http.ListenAndServe(":8080", nil))
}
