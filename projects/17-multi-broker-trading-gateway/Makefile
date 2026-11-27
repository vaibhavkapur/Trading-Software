.PHONY: setup build test bench demo reproduce validate clean

setup:
	./scripts/setup.sh

build:
	mkdir -p bin
	go build -o bin/gateway ./cmd/gateway

test:
	go vet ./...
	go test -race -count=1 -v ./...

bench:
	go test -run=^$$ -bench=. -benchmem ./tests/scenarios/...

demo:
	./scripts/demo.sh

reproduce:
	./scripts/reproduce.sh

validate:
	./scripts/validate.sh

clean:
	rm -rf bin data/runtime
