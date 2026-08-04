.PHONY: build typecheck clean format deploy

build:
	go build -o pi-msg .

typecheck:
	go vet ./...

clean:
	rm -f pi-msg

format:
	gofmt -w .

deploy:
	git push origin HEAD
	ssh naboo 'cd nixos-config && nix flake lock --update-input pi-msg && rebuild'
