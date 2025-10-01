FROM golang:alpine

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN go build -o main main.go

CMD ["./main"]