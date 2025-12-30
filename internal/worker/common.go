package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/Shavakan/runs-fleet/pkg/config"
	"github.com/Shavakan/runs-fleet/pkg/queue"
)

// MessageProcessor processes a single queue message.
type MessageProcessor func(ctx context.Context, msg queue.Message)

// RunWorkerLoop runs a generic worker loop that polls a queue and processes messages concurrently.
func RunWorkerLoop(ctx context.Context, name string, q queue.Queue, processor MessageProcessor) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	RunWorkerLoopWithTicker(ctx, name, q, processor, ticker.C)
}

// RunWorkerLoopWithTicker runs the worker loop with an injectable ticker for testing.
func RunWorkerLoopWithTicker(ctx context.Context, name string, q queue.Queue, processor MessageProcessor, tick <-chan time.Time) {
	log.Printf("Starting %s worker loop...", name)

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)
	var activeWork sync.WaitGroup

	defer func() {
		log.Printf("Waiting for in-flight %s work to complete...", name)
		activeWork.Wait()
		log.Printf("%s worker shutdown complete", name)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			timeout := 25 * time.Second
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining < timeout {
					timeout = remaining
				}
			}
			recvCtx, cancel := context.WithTimeout(ctx, timeout)
			messages, err := q.ReceiveMessages(recvCtx, 10, 20)
			cancel()
			if err != nil {
				log.Printf("Failed to receive messages: %v", err)
				continue
			}

			if len(messages) == 0 {
				continue
			}

			for _, msg := range messages {
				msg := msg
				activeWork.Add(1)
				go func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("panic in %s processMessage: %v", name, r)
						}
					}()
					defer activeWork.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					processCtx, processCancel := context.WithTimeout(ctx, config.MessageProcessTimeout)
					defer processCancel()
					processor(processCtx, msg)
				}()
			}
		}
	}
}
