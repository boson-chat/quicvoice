.PHONY: setup generate proto test vet run bot docker

setup:
	go mod download
	go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

generate: proto

proto:
	protoc -I proto --go_out=. --go_opt=module=github.com/boson-chat/quicvoice proto/control.proto

test:
	go test -race ./...

vet:
	go vet ./...

# Local server with a self-signed cert and no auth.
run:
	AUTH_INSECURE=true go run ./server/cmd serve

# Test client: make bot name=alice room=test
name ?= bot
room ?= test
addr ?= localhost:7700
bot:
	go run ./examples/bot -insecure -addr $(addr) -token $(name) -room $(room)

docker:
	docker build -t quicvoiced:dev .
