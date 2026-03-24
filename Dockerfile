FROM golang:1.24-alpine AS build
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o pressclips .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /app/pressclips .
COPY --from=build /app/index.html .
COPY --from=build /app/styles.css .
EXPOSE 8080
ENV PORT=8080
CMD ["./pressclips"]
