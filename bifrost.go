// Copyright 2019 VMware Inc.
package main

import (
    "bifrost/bus"
    "fmt"
)

func main() {

    bf := bus.GetBus()
    channel := "some-channel"
    bf.GetChannelManager().CreateChannel(channel)

    // listen for a single request on 'some-channel'
    requestHandler, _ := bf.ListenRequestStream(channel)
    requestHandler.Handle(
        func(msg *bus.Message) {
            pingContent := msg.Payload.(string)
            fmt.Printf("\nPing: %s\n", pingContent)

            // send a response back.
            bf.SendResponseMessage(channel, pingContent , msg.Id)
        },
        func(err error) {
            // something went wrong...
        })

    // send a request to 'some-channel' and handle a single response
    responseHandler, _ := bf.RequestOnce(channel, "Woo!")
    responseHandler.Handle(
        func(msg *bus.Message) {
            fmt.Printf("Pong: %s", msg.Payload.(string))
        },
        func(err error) {
            // something went wrong...
        })

    // fire the request.
    responseHandler.Fire()
}


