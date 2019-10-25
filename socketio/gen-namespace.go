// This file was automatically generated by genny.
// Any changes will be lost if this file is regenerated.
// see https://github.com/cheekybits/genny

package socketio

import (
	"bytes"
	"errors"
	"io"
	"sync"

	"github.com/golang/glog"
	"github.com/timpalpant/go-iex"
)

// The iexTOPSNamespace is a generic class built using Genny.
// https://github.com/cheekybits/genny
// Run "go generate" to re-generate the specific namespace types.

// Contains a channel for receiving namespace specific messages. Only messages
// for the symbols subscribed to will be passed along.
//
// The close method *must* be called before garbage collection.
type iexTOPSConnection struct {
	// For closing.
	sync.Once
	// Guards the closed value.
	sync.RWMutex

	// The ID of this endpoint. Used for removing it from the namespace.
	id int
	// A channel for passing along namespace specific messages.
	C chan iex.TOPS
	// Used to track which symbols this enpoint is subscribed to.
	subscriptions Subscriber
	// The factory function used to generate subscribe/unsubscribe messages.
	subUnsubMsgFactory subUnsubMsgFactory
	// Sends subscribe/unsubscribe structs to be encoded as JSON. When this
	// channel is closed, the connection is removed from the namespace.
	subUnsubClose chan<- *IEXMsg
	// True when this connection has been closed.
	closed bool
}

// Cleans up references to this connection in the Namespace. Messages will no
// longer be received and the Subscribe/Unsubscribe methods can no longer be
// called.
func (i *iexTOPSConnection) Close() {
	i.Do(func() {
		i.Lock()
		defer i.Unlock()
		i.closed = true
		close(i.subUnsubClose)
	})
}

// Subscribes to the given symbols. An error is returned if the connection is
// already closed.
func (i *iexTOPSConnection) Subscribe(symbols ...string) error {
	i.RLock()
	defer i.RUnlock()
	if i.closed {
		return errors.New(
			"Cannot call Subscribe on a closed connection")
	}
	go func() {
		for _, symbol := range symbols {
			i.subscriptions.Subscribe(symbol)
		}
		i.subUnsubClose <- i.subUnsubMsgFactory(
			Subscribe, symbols)
	}()
	return nil
}

// Unsubscribes to the given symbols. An error is returned if the connection is
// already closed.
func (i *iexTOPSConnection) Unsubscribe(symbols ...string) error {
	i.RLock()
	defer i.RUnlock()
	if i.closed {
		return errors.New(
			"Cannot call Unsubscribe on a closed connection")
	}
	go func() {
		for _, symbol := range symbols {
			i.subscriptions.Unsubscribe(symbol)
		}
		i.subUnsubClose <- i.subUnsubMsgFactory(
			Unsubscribe, symbols)
	}()
	return nil
}

// Returns true if this connection is subscribed to the given symbol.
func (i *iexTOPSConnection) Subscribed(symbol string) bool {
	return i.subscriptions.Subscribed(symbol)
}

// Receives messages for a given namespace and forwards them to endpoints.
type iexTOPSNamespace struct {
	// Used to guard access to the fanout channels.
	sync.RWMutex

	// The ID to use for the next endpoint created.
	nextId int
	// Active endpoints by ID.
	connections map[int]*iexTOPSConnection
	// Receives raw messages from the Transport. Only messages for the
	// current namespace will be received.
	msgChannel <-chan packetMetadata
	// For encoding outgoing messages in this namespace.
	encoder Encoder
	// Used for sending messages to IEX SocketIO.
	writer io.Writer
	// The factory function used to generate subscribe/unsubscribe messages.
	subUnsubMsgFactory subUnsubMsgFactory
	// A function to be called when the namespace has no more endpoints.
	closeFunc func()
}

func (i *iexTOPSNamespace) writeToReader(r io.Reader) error {
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(r); err != nil {
		return err
	}
	if glog.V(3) {
		glog.Infof("Writing '%s' to reader", buffer.String())
	}
	if _, err := buffer.WriteTo(i.writer); err != nil {
		return err
	}
	return nil
}

// Sends a subscribe message and starts listening for incoming data. This is
// called when the namespace is created.
func (i *iexTOPSNamespace) connect() error {
	r, err := i.encoder.EncodePacket(Message, Connect)
	if err != nil {
		return err
	}
	if err := i.writeToReader(r); err != nil {
		return err
	}
	// Start listening for messages from the Transport layer.
	go func() {
		for msg := range i.msgChannel {
			i.fanout(msg)
		}
		// Close all outgoing connections.
		i.RLock()
		defer i.RUnlock()
		for _, connection := range i.connections {
			close(connection.C)
		}
	}()
	return nil
}

// Given a string representing a JSON IEX message type, parse out the symbol and
// the message and pass the message to each connection subscribed to the symbol.
// Use a go routine to prevent from blocking.
func (i *iexTOPSNamespace) fanout(pkt packetMetadata) {
	go func() {
		// This "symbol only" struct is necessary because this class
		// is a genny generic. Therefore, even though all IEX messages
		// have a "symbol" field, iexTOPS.symbol is not type safe.
		var symbol struct {
			Symbol string
		}
		if err := ParseToJSON(pkt.Data, &symbol); err != nil {
			glog.Errorf("No symbol found for IexTOPS: %s - %v",
				err, pkt)
		}
		// Now that the symbol has been extraced, the specific message
		// can be extracted from the data.
		var decoded iex.TOPS
		if err := ParseToJSON(pkt.Data, &decoded); err != nil {
			glog.Errorf("Could not decode IexTOPS: %s - %v",
				err, pkt)
		}
		i.RLock()
		defer i.RUnlock()
		for _, connection := range i.connections {
			if connection.Subscribed(symbol.Symbol) {
				connection.C <- decoded
			}
		}
	}()
}

// Returns a connection that will receive messages for the passed in symbols.
// If no symbols are passed in, they can be added/removed later.
func (i *iexTOPSNamespace) GetConnection(
	symbols ...string) *iexTOPSConnection {
	i.Lock()
	defer i.Unlock()
	i.nextId++
	subUnsubClose := make(chan *IEXMsg, 0)
	connection := &iexTOPSConnection{
		id:                 i.nextId,
		C:                  make(chan iex.TOPS, 1),
		subscriptions:      NewPresenceSubscriber(),
		subUnsubMsgFactory: i.subUnsubMsgFactory,
		subUnsubClose:      subUnsubClose,
		closed:             false,
	}
	// Start listening for close, subscribe and unsubscribe messages on the
	// new connection.
	go func(id int) {
		for subUnsubMsg := range subUnsubClose {
			r, err := i.encoder.EncodeMsg(
				Message, Event, subUnsubMsg)
			if err != nil {
				glog.Errorf("Error encoding %+v: %s",
					subUnsubMsg, err)
				continue
			}
			if err := i.writeToReader(r); err != nil {
				glog.Errorf("Error encoding %+v: %s",
					subUnsubMsg, err)
				continue
			}

		}
		i.Lock()
		defer i.Unlock()
		delete(i.connections, id)
		if len(i.connections) == 0 {
			i.closeFunc()
		}

	}(i.nextId)
	i.connections[i.nextId] = connection
	if len(symbols) > 0 {
		connection.Subscribe(symbols...)
	}
	return connection
}

func newIexTOPSNamespace(
	ch <-chan packetMetadata, encoder Encoder,
	writer io.Writer, subUnsubMsgFactory subUnsubMsgFactory,
	closeFunc func()) *iexTOPSNamespace {
	newNs := &iexTOPSNamespace{
		nextId:             0,
		connections:        make(map[int]*iexTOPSConnection),
		msgChannel:         ch,
		encoder:            encoder,
		writer:             writer,
		subUnsubMsgFactory: subUnsubMsgFactory,
		closeFunc:          closeFunc,
	}
	newNs.connect()
	return newNs
}

// The iexLastNamespace is a generic class built using Genny.
// https://github.com/cheekybits/genny
// Run "go generate" to re-generate the specific namespace types.

// Contains a channel for receiving namespace specific messages. Only messages
// for the symbols subscribed to will be passed along.
//
// The close method *must* be called before garbage collection.
type iexLastConnection struct {
	// For closing.
	sync.Once
	// Guards the closed value.
	sync.RWMutex

	// The ID of this endpoint. Used for removing it from the namespace.
	id int
	// A channel for passing along namespace specific messages.
	C chan iex.Last
	// Used to track which symbols this enpoint is subscribed to.
	subscriptions Subscriber
	// The factory function used to generate subscribe/unsubscribe messages.
	subUnsubMsgFactory subUnsubMsgFactory
	// Sends subscribe/unsubscribe structs to be encoded as JSON. When this
	// channel is closed, the connection is removed from the namespace.
	subUnsubClose chan<- *IEXMsg
	// True when this connection has been closed.
	closed bool
}

// Cleans up references to this connection in the Namespace. Messages will no
// longer be received and the Subscribe/Unsubscribe methods can no longer be
// called.
func (i *iexLastConnection) Close() {
	i.Do(func() {
		i.Lock()
		defer i.Unlock()
		i.closed = true
		close(i.subUnsubClose)
	})
}

// Subscribes to the given symbols. An error is returned if the connection is
// already closed.
func (i *iexLastConnection) Subscribe(symbols ...string) error {
	i.RLock()
	defer i.RUnlock()
	if i.closed {
		return errors.New(
			"Cannot call Subscribe on a closed connection")
	}
	go func() {
		for _, symbol := range symbols {
			i.subscriptions.Subscribe(symbol)
		}
		i.subUnsubClose <- i.subUnsubMsgFactory(
			Subscribe, symbols)
	}()
	return nil
}

// Unsubscribes to the given symbols. An error is returned if the connection is
// already closed.
func (i *iexLastConnection) Unsubscribe(symbols ...string) error {
	i.RLock()
	defer i.RUnlock()
	if i.closed {
		return errors.New(
			"Cannot call Unsubscribe on a closed connection")
	}
	go func() {
		for _, symbol := range symbols {
			i.subscriptions.Unsubscribe(symbol)
		}
		i.subUnsubClose <- i.subUnsubMsgFactory(
			Unsubscribe, symbols)
	}()
	return nil
}

// Returns true if this connection is subscribed to the given symbol.
func (i *iexLastConnection) Subscribed(symbol string) bool {
	return i.subscriptions.Subscribed(symbol)
}

// Receives messages for a given namespace and forwards them to endpoints.
type iexLastNamespace struct {
	// Used to guard access to the fanout channels.
	sync.RWMutex

	// The ID to use for the next endpoint created.
	nextId int
	// Active endpoints by ID.
	connections map[int]*iexLastConnection
	// Receives raw messages from the Transport. Only messages for the
	// current namespace will be received.
	msgChannel <-chan packetMetadata
	// For encoding outgoing messages in this namespace.
	encoder Encoder
	// Used for sending messages to IEX SocketIO.
	writer io.Writer
	// The factory function used to generate subscribe/unsubscribe messages.
	subUnsubMsgFactory subUnsubMsgFactory
	// A function to be called when the namespace has no more endpoints.
	closeFunc func()
}

func (i *iexLastNamespace) writeToReader(r io.Reader) error {
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(r); err != nil {
		return err
	}
	if glog.V(3) {
		glog.Infof("Writing '%s' to reader", buffer.String())
	}
	if _, err := buffer.WriteTo(i.writer); err != nil {
		return err
	}
	return nil
}

// Sends a subscribe message and starts listening for incoming data. This is
// called when the namespace is created.
func (i *iexLastNamespace) connect() error {
	r, err := i.encoder.EncodePacket(Message, Connect)
	if err != nil {
		return err
	}
	if err := i.writeToReader(r); err != nil {
		return err
	}
	// Start listening for messages from the Transport layer.
	go func() {
		for msg := range i.msgChannel {
			i.fanout(msg)
		}
		// Close all outgoing connections.
		i.RLock()
		defer i.RUnlock()
		for _, connection := range i.connections {
			close(connection.C)
		}
	}()
	return nil
}

// Given a string representing a JSON IEX message type, parse out the symbol and
// the message and pass the message to each connection subscribed to the symbol.
// Use a go routine to prevent from blocking.
func (i *iexLastNamespace) fanout(pkt packetMetadata) {
	go func() {
		// This "symbol only" struct is necessary because this class
		// is a genny generic. Therefore, even though all IEX messages
		// have a "symbol" field, iexLast.symbol is not type safe.
		var symbol struct {
			Symbol string
		}
		if err := ParseToJSON(pkt.Data, &symbol); err != nil {
			glog.Errorf("No symbol found for IexLast: %s - %v",
				err, pkt)
		}
		// Now that the symbol has been extraced, the specific message
		// can be extracted from the data.
		var decoded iex.Last
		if err := ParseToJSON(pkt.Data, &decoded); err != nil {
			glog.Errorf("Could not decode IexLast: %s - %v",
				err, pkt)
		}
		i.RLock()
		defer i.RUnlock()
		for _, connection := range i.connections {
			if connection.Subscribed(symbol.Symbol) {
				connection.C <- decoded
			}
		}
	}()
}

// Returns a connection that will receive messages for the passed in symbols.
// If no symbols are passed in, they can be added/removed later.
func (i *iexLastNamespace) GetConnection(
	symbols ...string) *iexLastConnection {
	i.Lock()
	defer i.Unlock()
	i.nextId++
	subUnsubClose := make(chan *IEXMsg, 0)
	connection := &iexLastConnection{
		id:                 i.nextId,
		C:                  make(chan iex.Last, 1),
		subscriptions:      NewPresenceSubscriber(),
		subUnsubMsgFactory: i.subUnsubMsgFactory,
		subUnsubClose:      subUnsubClose,
		closed:             false,
	}
	// Start listening for close, subscribe and unsubscribe messages on the
	// new connection.
	go func(id int) {
		for subUnsubMsg := range subUnsubClose {
			r, err := i.encoder.EncodeMsg(
				Message, Event, subUnsubMsg)
			if err != nil {
				glog.Errorf("Error encoding %+v: %s",
					subUnsubMsg, err)
				continue
			}
			if err := i.writeToReader(r); err != nil {
				glog.Errorf("Error encoding %+v: %s",
					subUnsubMsg, err)
				continue
			}

		}
		i.Lock()
		defer i.Unlock()
		delete(i.connections, id)
		if len(i.connections) == 0 {
			i.closeFunc()
		}

	}(i.nextId)
	i.connections[i.nextId] = connection
	if len(symbols) > 0 {
		connection.Subscribe(symbols...)
	}
	return connection
}

func newIexLastNamespace(
	ch <-chan packetMetadata, encoder Encoder,
	writer io.Writer, subUnsubMsgFactory subUnsubMsgFactory,
	closeFunc func()) *iexLastNamespace {
	newNs := &iexLastNamespace{
		nextId:             0,
		connections:        make(map[int]*iexLastConnection),
		msgChannel:         ch,
		encoder:            encoder,
		writer:             writer,
		subUnsubMsgFactory: subUnsubMsgFactory,
		closeFunc:          closeFunc,
	}
	newNs.connect()
	return newNs
}

// The iexDEEPNamespace is a generic class built using Genny.
// https://github.com/cheekybits/genny
// Run "go generate" to re-generate the specific namespace types.

// Contains a channel for receiving namespace specific messages. Only messages
// for the symbols subscribed to will be passed along.
//
// The close method *must* be called before garbage collection.
type iexDEEPConnection struct {
	// For closing.
	sync.Once
	// Guards the closed value.
	sync.RWMutex

	// The ID of this endpoint. Used for removing it from the namespace.
	id int
	// A channel for passing along namespace specific messages.
	C chan iex.DEEP
	// Used to track which symbols this enpoint is subscribed to.
	subscriptions Subscriber
	// The factory function used to generate subscribe/unsubscribe messages.
	subUnsubMsgFactory subUnsubMsgFactory
	// Sends subscribe/unsubscribe structs to be encoded as JSON. When this
	// channel is closed, the connection is removed from the namespace.
	subUnsubClose chan<- *IEXMsg
	// True when this connection has been closed.
	closed bool
}

// Cleans up references to this connection in the Namespace. Messages will no
// longer be received and the Subscribe/Unsubscribe methods can no longer be
// called.
func (i *iexDEEPConnection) Close() {
	i.Do(func() {
		i.Lock()
		defer i.Unlock()
		i.closed = true
		close(i.subUnsubClose)
	})
}

// Subscribes to the given symbols. An error is returned if the connection is
// already closed.
func (i *iexDEEPConnection) Subscribe(symbols ...string) error {
	i.RLock()
	defer i.RUnlock()
	if i.closed {
		return errors.New(
			"Cannot call Subscribe on a closed connection")
	}
	go func() {
		for _, symbol := range symbols {
			i.subscriptions.Subscribe(symbol)
		}
		i.subUnsubClose <- i.subUnsubMsgFactory(
			Subscribe, symbols)
	}()
	return nil
}

// Unsubscribes to the given symbols. An error is returned if the connection is
// already closed.
func (i *iexDEEPConnection) Unsubscribe(symbols ...string) error {
	i.RLock()
	defer i.RUnlock()
	if i.closed {
		return errors.New(
			"Cannot call Unsubscribe on a closed connection")
	}
	go func() {
		for _, symbol := range symbols {
			i.subscriptions.Unsubscribe(symbol)
		}
		i.subUnsubClose <- i.subUnsubMsgFactory(
			Unsubscribe, symbols)
	}()
	return nil
}

// Returns true if this connection is subscribed to the given symbol.
func (i *iexDEEPConnection) Subscribed(symbol string) bool {
	return i.subscriptions.Subscribed(symbol)
}

// Receives messages for a given namespace and forwards them to endpoints.
type iexDEEPNamespace struct {
	// Used to guard access to the fanout channels.
	sync.RWMutex

	// The ID to use for the next endpoint created.
	nextId int
	// Active endpoints by ID.
	connections map[int]*iexDEEPConnection
	// Receives raw messages from the Transport. Only messages for the
	// current namespace will be received.
	msgChannel <-chan packetMetadata
	// For encoding outgoing messages in this namespace.
	encoder Encoder
	// Used for sending messages to IEX SocketIO.
	writer io.Writer
	// The factory function used to generate subscribe/unsubscribe messages.
	subUnsubMsgFactory subUnsubMsgFactory
	// A function to be called when the namespace has no more endpoints.
	closeFunc func()
}

func (i *iexDEEPNamespace) writeToReader(r io.Reader) error {
	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(r); err != nil {
		return err
	}
	if glog.V(3) {
		glog.Infof("Writing '%s' to reader", buffer.String())
	}
	if _, err := buffer.WriteTo(i.writer); err != nil {
		return err
	}
	return nil
}

// Sends a subscribe message and starts listening for incoming data. This is
// called when the namespace is created.
func (i *iexDEEPNamespace) connect() error {
	r, err := i.encoder.EncodePacket(Message, Connect)
	if err != nil {
		return err
	}
	if err := i.writeToReader(r); err != nil {
		return err
	}
	// Start listening for messages from the Transport layer.
	go func() {
		for msg := range i.msgChannel {
			i.fanout(msg)
		}
		// Close all outgoing connections.
		i.RLock()
		defer i.RUnlock()
		for _, connection := range i.connections {
			close(connection.C)
		}
	}()
	return nil
}

// Given a string representing a JSON IEX message type, parse out the symbol and
// the message and pass the message to each connection subscribed to the symbol.
// Use a go routine to prevent from blocking.
func (i *iexDEEPNamespace) fanout(pkt packetMetadata) {
	go func() {
		// This "symbol only" struct is necessary because this class
		// is a genny generic. Therefore, even though all IEX messages
		// have a "symbol" field, iexDEEP.symbol is not type safe.
		var symbol struct {
			Symbol string
		}
		if err := ParseToJSON(pkt.Data, &symbol); err != nil {
			glog.Errorf("No symbol found for IexDEEP: %s - %v",
				err, pkt)
		}
		// Now that the symbol has been extraced, the specific message
		// can be extracted from the data.
		var decoded iex.DEEP
		if err := ParseToJSON(pkt.Data, &decoded); err != nil {
			glog.Errorf("Could not decode IexDEEP: %s - %v",
				err, pkt)
		}
		i.RLock()
		defer i.RUnlock()
		for _, connection := range i.connections {
			if connection.Subscribed(symbol.Symbol) {
				connection.C <- decoded
			}
		}
	}()
}

// Returns a connection that will receive messages for the passed in symbols.
// If no symbols are passed in, they can be added/removed later.
func (i *iexDEEPNamespace) GetConnection(
	symbols ...string) *iexDEEPConnection {
	i.Lock()
	defer i.Unlock()
	i.nextId++
	subUnsubClose := make(chan *IEXMsg, 0)
	connection := &iexDEEPConnection{
		id:                 i.nextId,
		C:                  make(chan iex.DEEP, 1),
		subscriptions:      NewPresenceSubscriber(),
		subUnsubMsgFactory: i.subUnsubMsgFactory,
		subUnsubClose:      subUnsubClose,
		closed:             false,
	}
	// Start listening for close, subscribe and unsubscribe messages on the
	// new connection.
	go func(id int) {
		for subUnsubMsg := range subUnsubClose {
			r, err := i.encoder.EncodeMsg(
				Message, Event, subUnsubMsg)
			if err != nil {
				glog.Errorf("Error encoding %+v: %s",
					subUnsubMsg, err)
				continue
			}
			if err := i.writeToReader(r); err != nil {
				glog.Errorf("Error encoding %+v: %s",
					subUnsubMsg, err)
				continue
			}

		}
		i.Lock()
		defer i.Unlock()
		delete(i.connections, id)
		if len(i.connections) == 0 {
			i.closeFunc()
		}

	}(i.nextId)
	i.connections[i.nextId] = connection
	if len(symbols) > 0 {
		connection.Subscribe(symbols...)
	}
	return connection
}

func newIexDEEPNamespace(
	ch <-chan packetMetadata, encoder Encoder,
	writer io.Writer, subUnsubMsgFactory subUnsubMsgFactory,
	closeFunc func()) *iexDEEPNamespace {
	newNs := &iexDEEPNamespace{
		nextId:             0,
		connections:        make(map[int]*iexDEEPConnection),
		msgChannel:         ch,
		encoder:            encoder,
		writer:             writer,
		subUnsubMsgFactory: subUnsubMsgFactory,
		closeFunc:          closeFunc,
	}
	newNs.connect()
	return newNs
}
