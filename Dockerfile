FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /massdriver-automations ./cmd/massdriver-automations

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /massdriver-automations /massdriver-automations
EXPOSE 8080
ENTRYPOINT ["/massdriver-automations"]
