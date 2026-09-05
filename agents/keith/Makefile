.DEFAULT_GOAL := help

.PHONY: help doctor setup dev check test build image up down logs scaffold-plugin scaffold-skill

help:
	@./keith help

doctor setup dev check test build image up down logs:
	@./keith $@

scaffold-plugin:
	@test -n "$(NAME)" || (echo "usage: make scaffold-plugin NAME=my-plugin" >&2; exit 2)
	@./keith scaffold plugin "$(NAME)"

scaffold-skill:
	@test -n "$(NAME)" || (echo "usage: make scaffold-skill NAME=my-skill" >&2; exit 2)
	@./keith scaffold skill "$(NAME)"

