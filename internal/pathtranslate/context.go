package pathtranslate

import "context"

type contextKey struct{}

func WithTranslator(ctx context.Context, translator *Translator) context.Context {
	return context.WithValue(ctx, contextKey{}, translator)
}

func FromContext(ctx context.Context) *Translator {
	translator, _ := ctx.Value(contextKey{}).(*Translator)
	return translator
}
