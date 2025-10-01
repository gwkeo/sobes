package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
)

var (
	admins = []int{0, 1, 2, 3, 4}
	users  = []int{100, 200, 300, 400}
)

func getAdmins() []int {
	return admins
}

func getUsers() []int {
	return users
}

type incidentRequest struct {
	Message string `json:"message"`
}

type AccessRequest struct {
	ID int `json:"id"`
}

func ErrorResponse(errMessage string) []byte {
	return fmt.Appendf(nil, `"error":"%s"`, errMessage)
}

type notificationsService struct {
	client *kgo.Client
}

func NewClient(addr string) *notificationsService {
	client, err := kgo.NewClient(
		kgo.SeedBrokers(addr),
	)
	if err != nil {
		log.Fatal(err)
	}

	return &notificationsService{
		client: client,
	}
}

func (s *notificationsService) Close() {
	s.client.Close()
}

func (s *notificationsService) NewIncident(ctx context.Context, uuid uuid.UUID, message string) ([]byte, error) {
	topicName := "incidents.opened"

	record := kgo.Record{
		Topic: topicName,
		Key:   fmt.Appendf(nil, "inc-{%s}", uuid),
		Value: fmt.Appendf(nil, `{"message":"%s"}`, message),
	}

	result, err := s.client.ProduceSync(ctx, &record).First()
	if err != nil {
		return nil, err
	}

	return result.Key, nil
}

func (s *notificationsService) NewAccessRequest(ctx context.Context, uuid uuid.UUID) {
	// topicName := "access.requests.accepted"
}

func (s *notificationsService) ServeLastNotification(ctx context.Context) {

}

func main() {
	client := NewClient("kafka:9092")

	mux := http.NewServeMux()
	mux.HandleFunc("/incidents/new", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		UUID, err := uuid.NewUUID()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			w.Write(ErrorResponse("invalid request body"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var body incidentRequest
		err = json.Unmarshal(rawBody, &body)
		if err != nil {
			w.Write(ErrorResponse("unable to unmarshal request body"))
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		key, err := client.NewIncident(ctx, UUID, body.Message)
		if err != nil {
			w.Write(ErrorResponse(err.Error()))
			w.WriteHeader(http.StatusBadRequest)
		}

		w.Write(key)
	})

	if err := http.ListenAndServe("localhost:80", mux); err != nil {
		panic(err)
	}

	// client, err := kgo.NewClient(
	// 	kgo.SeedBrokers(""),
	// )
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// defer client.Close()

	// ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	// defer cancel()

	// if err := client.Ping(ctx); err != nil {
	// 	log.Fatal(err)
	// }

	// record := kgo.Record{
	// 	Topic: "orders.created",
	// 	Key:   []byte("order-101"),
	// 	Value: []byte(`{"event_id":"e-go-1","order_id":"order-101","type":"OrderCreated"}`),
	// }

	// written, err := client.ProduceSync(ctx, &record).First()
	// if err != nil {
	// 	log.Fatal(err)
	// }

	// fmt.Println(written.Topic, written.Key, written.Value)

	fmt.Println(getUsers())
}
