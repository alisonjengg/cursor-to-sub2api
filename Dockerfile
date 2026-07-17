# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cursor-to-sub2api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cursor-to-sub2api /cursor-to-sub2api
EXPOSE 8080
USER nonroot:nonroot
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/cursor-to-sub2api", "healthcheck"]
ENTRYPOINT ["/cursor-to-sub2api"]
