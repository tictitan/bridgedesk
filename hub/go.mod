module bridgedesk

go 1.22

require (
	github.com/gorilla/websocket v1.5.3
	golang.org/x/net v0.0.0
)

// Dependencies are vendored as source in third_party/ so the project builds fully offline.
replace github.com/gorilla/websocket => ./third_party/websocket

replace golang.org/x/net => ./third_party/xnet
