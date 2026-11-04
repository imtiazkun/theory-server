# theory-server: one Go program in an image that includes git (GIT-10).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /theory-server .

FROM alpine:3.22
RUN apk add --no-cache git \
	&& git config --system --add safe.directory '*'
COPY --from=build /theory-server /usr/local/bin/theory-server
ENV THEORY_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["theory-server"]
CMD ["serve", "--addr", ":8080"]
