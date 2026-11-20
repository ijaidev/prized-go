// Package prized is an independent Go client for Prized Sandboxes.
//
// A Client holds authentication and transport settings; its Sandboxes service
// creates, connects to, lists and deletes sandboxes. A Sandbox handle exposes
// the fields of the last API answer, runs foreground commands and deletes the
// sandbox it names.
package prized
