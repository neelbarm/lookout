GO      ?= go
BIN     := bin
PREFIX  ?= /usr/local
DEMO_SECS ?= 90

.PHONY: all build test vet fmt demo demo-file install clean check

all: build

build: $(BIN)/lookout $(BIN)/loggen

$(BIN)/lookout: $(shell find . -name '*.go' -not -name '*_test.go')
	@mkdir -p $(BIN)
	$(GO) build -trimpath -o $(BIN)/lookout ./cmd/lookout

$(BIN)/loggen: $(shell find . -name '*.go' -not -name '*_test.go')
	@mkdir -p $(BIN)
	$(GO) build -trimpath -o $(BIN)/loggen ./cmd/loggen

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

check: fmt vet test build

## demo: a live 90-second dashboard with a scripted incident at t+40s
demo: build
	@echo "lookout live demo: press q to quit, p to pause, a for anomalies only"
	@./$(BIN)/loggen --duration $(DEMO_SECS)s --rate 60 --seed 7 | ./$(BIN)/lookout

## demo-file: write examples/incident.log and print the offline report
demo-file: build
	@mkdir -p examples
	./$(BIN)/loggen --duration $(DEMO_SECS)s --rate 60 --seed 7 --file examples/incident.log
	@echo
	./$(BIN)/lookout report examples/incident.log

## demo-json: the same incident through the piped, JSON-lines path
demo-json: build
	@./$(BIN)/loggen --duration $(DEMO_SECS)s --rate 60 --seed 7 --file /tmp/lookout-demo.log
	@./$(BIN)/lookout replay /tmp/lookout-demo.log --speed 1000 --json

install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(BIN)/lookout $(PREFIX)/bin/lookout
	install -m 0755 $(BIN)/loggen $(PREFIX)/bin/loggen

clean:
	rm -rf $(BIN) examples/incident.log
