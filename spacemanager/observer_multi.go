package spacemanager

// ComposeObservers fans one StateEvent stream out to several observers in
// order. nil observers are skipped. Used to drive the CLI reporter and the
// OTEL exporter from the agent's single Options.Observer sink without the
// agent knowing there is more than one consumer.
func ComposeObservers(obs ...StateObserver) StateObserver {
	live := make([]StateObserver, 0, len(obs))
	for _, o := range obs {
		if o != nil {
			live = append(live, o)
		}
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0]
	}
	return func(e StateEvent) {
		for _, o := range live {
			o(e)
		}
	}
}
