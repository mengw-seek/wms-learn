.PHONY: run build test tidy compose-up compose-down

run:
	go run ./cmd/wms

build:
	go build -o bin/wms.exe ./cmd/wms

test:
	go test ./... -v

tidy:
	go mod tidy

compose-up:
	docker compose -f deploy/docker-compose.yaml up -d

compose-down:
	docker compose -f deploy/docker-compose.yaml down
