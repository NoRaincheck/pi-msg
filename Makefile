.PHONY: deploy

# Deploy the latest committed pi-msg to the naboo NixOS host (runs beltino).
# Pushes the current branch (so the flake input can resolve the new commit),
# then on the server bumps the pi-msg flake input and rebuilds.
deploy:
	git push origin HEAD
	ssh naboo 'cd nixos-config && nix flake lock --update-input pi-msg && rebuild'
