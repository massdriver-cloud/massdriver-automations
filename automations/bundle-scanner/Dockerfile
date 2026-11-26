# Build from the repository root: docker build -f automations/bundle-scanner/Dockerfile .
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /massdriver-automations ./cmd/massdriver-automations

FROM aquasec/trivy:0.76.2 AS trivy

FROM bridgecrew/checkov:3.4.6
COPY --from=trivy /usr/local/bin/trivy /usr/local/bin/trivy
COPY --from=build /massdriver-automations /massdriver-automations
RUN useradd --uid 65532 --create-home --home-dir /home/scanner scanner
USER 65532
ENV HOME=/home/scanner
EXPOSE 8080
ENTRYPOINT ["/massdriver-automations"]
CMD ["run", "bundle-scanner"]
