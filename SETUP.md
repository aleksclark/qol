We're going to set up a new project: Qol

go: 1.26.6

cobra & viper on all cmds

we have some initial interfaces designed:
core.go - root domain types
payloads.go - what kinds of payloads

these may need to be moved around to conformed to our go domain package layout. all wire formats must be protobuf

# docker
docker-compose.dev.yml - dev compose, exposes web SPA(s), all services run via `air` so we get hot reloads
docker-compose.e2e.yml - e2e yml, no exposed ports

# services

* web - main UI, vite/react/typescript, webtransport conn to qol-api
* qol-api - system control plane & NATS <-> WebTransport gateway
* qol-worker - event handler(s), in docker we want 2 instances
* nats

# persistence
To keep stuff simple, write a thin `db` interface around NATS k/v to persist things like user accounts, session records, etc

# procedure

Set up initial project structure, demonstrate the following flows:

1. admin account bootstrap
2. login flow
3. admin ui streams mp3 file to qol-api -> qol-api decodes to PCM for the input stage -> qol-worker echos PCM into output stage -> qol-api observes events from output stage
