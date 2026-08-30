package singleserver

import (
	"bufio"
	"io"
)

// Sequential prompters must share one buffered reader per input source: a
// fresh bufio.Reader would swallow input the previous one had buffered ahead
// (piped answers, terminal type-ahead).
var sharedPromptReader struct {
	src io.Reader
	r   *bufio.Reader
}

func promptReaderFor(input io.Reader) *bufio.Reader {
	if sharedPromptReader.src != input {
		sharedPromptReader.src = input
		sharedPromptReader.r = bufio.NewReader(input)
	}
	return sharedPromptReader.r
}

func interactivePrompter(w io.Writer) addPrompter {
	return addPrompter{reader: promptReaderFor(addPromptInput), w: w}
}
