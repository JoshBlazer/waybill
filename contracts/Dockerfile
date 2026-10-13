# syntax=docker/dockerfile:1
# Contracts compiled at image build time, so deploying to a local chain at
# `make up` needs no network (no compiler download) and survives `make down`.
FROM ghcr.io/foundry-rs/foundry:v1.8.3
WORKDIR /contracts
COPY --chown=foundry:foundry . .
# deployments/ exists in the image owned by foundry, so the named volume
# mounted over it starts with the same owner.
RUN mkdir -p deployments && forge build
ENTRYPOINT ["forge", "script", "script/Deploy.s.sol", "--broadcast"]
