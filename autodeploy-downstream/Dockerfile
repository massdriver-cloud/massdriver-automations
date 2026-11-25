FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /autodeploy-downstream .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /autodeploy-downstream /autodeploy-downstream
EXPOSE 8080
ENTRYPOINT ["/autodeploy-downstream"]
