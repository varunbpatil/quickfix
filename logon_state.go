// Copyright (c) quickfixengine.org  All rights reserved.
//
// This file may be distributed under the terms of the quickfixengine.org
// license as defined by quickfixengine.org and appearing in the file
// LICENSE included in the packaging of this file.
//
// This file is provided AS IS with NO WARRANTY OF ANY KIND, INCLUDING
// THE WARRANTY OF DESIGN, MERCHANTABILITY AND FITNESS FOR A
// PARTICULAR PURPOSE.
//
// See http://www.quickfixengine.org/LICENSE for licensing information.
//
// Contact ask@quickfixengine.org if any conditions of this licensing
// are not clear to you.

package quickfix

import (
	"bytes"
	"regexp"
	"strconv"

	"github.com/quickfixgo/quickfix/internal"
)

var msgSeqNumTooLowRegex = regexp.MustCompile(`MsgSeqNum too low, expecting (\d+) but received \d+`)

type logonState struct{ connectedNotLoggedOn }

func (s logonState) String() string { return "Logon State" }

func (s logonState) FixMsgIn(session *session, msg *Message) (nextState sessionState) {
	msgType, err := msg.Header.GetBytes(tagMsgType)
	if err != nil {
		return handleStateError(session, err)
	}

	// If we receive a logout while in the logon state and the reason for logout is the message
	// sequence number being too low, it means the sender has lost session state (testing?). There
	// is no recovering from lost data, so, if the application wants to, we can force the next
	// sender message sequence number to be equal to whatever the target expects it to be. Without
	// this, the application will have to wait until the sender message sequence number naturally
	// matches what the target is expecting (as a result of retrying logon multiple times) and then
	// the logon will succeed, but this could take several hours depending on the sequence num gap.
	// This is almost certainly required in testing where the sender session state is discarded
	// after testing (assuming the target doesn't support ResetSeqNumFlag 141=Y).
	if bytes.Equal(msgType, msgTypeLogout) {
		session.log.OnEventf("Invalid Session State: Received Logout %s while waiting for Logon", msg)

		// Get the reason for logout.
		reason, err := msg.Body.GetString(tagText)
		if err != nil {
			return handleStateError(session, err)
		}

		// Check if the reason is message sequence number being too low.
		res := msgSeqNumTooLowRegex.FindStringSubmatch(reason)
		if res == nil {
			return latentState{}
		}

		// Message sequence number is too low.
		if session.LogonForceSenderMsgSeqNum {
			// Get the value expected by the target.
			expectedMsgSeqNum, err := strconv.Atoi(res[1])
			if err != nil {
				return handleStateError(session, err)
			}

			session.log.OnEventf("Forcing next sender message sequence number to %d", expectedMsgSeqNum)

			// Force the next sender message sequence number to be equal to the expected value.
			if err := session.forceNextSenderMsgSeqNum(expectedMsgSeqNum); err != nil {
				return handleStateError(session, err)
			}
		}

		return latentState{}
	}

	if !bytes.Equal(msgType, msgTypeLogon) {
		session.log.OnEventf("Invalid Session State: Received Msg %s while waiting for Logon", msg)
		return latentState{}
	}

	if err := session.handleLogon(msg); err != nil {
		switch err := err.(type) {
		case RejectLogon:
			return shutdownWithReason(session, msg, true, err.Error())

		case targetTooLow:
			return shutdownWithReason(session, msg, false, err.Error())

		case targetTooHigh:
			var tooHighErr error
			if nextState, tooHighErr = session.doTargetTooHigh(err); tooHighErr != nil {
				return shutdownWithReason(session, msg, false, tooHighErr.Error())
			}

			return

		default:
			return handleStateError(session, err)
		}
	}

	// Notify the app that the session is ready.
	session.application.InSession(session.sessionID)

	return inSession{}
}

func (s logonState) Timeout(session *session, e internal.Event) (nextState sessionState) {
	switch e {
	case internal.LogonTimeout:
		session.log.OnEvent("Timed out waiting for logon response")
		return latentState{}
	}
	return s
}

func (s logonState) Stop(session *session) (nextState sessionState) {
	return latentState{}
}

func shutdownWithReason(session *session, msg *Message, incrNextTargetMsgSeqNum bool, reason string) (nextState sessionState) {
	session.log.OnEvent(reason)
	logout := session.buildLogout(reason)

	if err := session.dropAndSendInReplyTo(logout, msg); err != nil {
		session.logError(err)
	}

	if incrNextTargetMsgSeqNum {
		if err := session.store.IncrNextTargetMsgSeqNum(); err != nil {
			session.logError(err)
		}
	}

	return latentState{}
}
