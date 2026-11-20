// Package prized is an independent Go client for Prized Sandboxes.
//
// A Client holds authentication and transport settings; its Sandboxes service
// creates, connects to, lists and deletes sandboxes. A Sandbox handle exposes
// the fields of the last API answer, runs commands in the foreground or
// background, moves files in and out, exposes ports at preview URLs, and
// deletes the sandbox it names.
package prized
