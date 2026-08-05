build:
	go build -buildvcs=false -o pi-msg .

# Discover the local Bonjour XMPP service, list the users online, and print the jid/owner to put in config
discover:
	go build -buildvcs=false -o pi-msg .
	./pi-msg --discover

typecheck:
	go vet ./...

clean:
	rm -f pi-msg

format:
	gofmt -w .
