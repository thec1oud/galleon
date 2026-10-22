package galleon

type Registry struct{}

func Config[T any](r *Registry, section string) *T { return new(T) }
