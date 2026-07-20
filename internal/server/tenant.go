package server

import "context"

// tenantContextKey is deliberately private so callers cannot inject an
// unverified tenant namespace. The API-key middleware is the only writer.
type tenantContextKey struct{}

func withTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		tenant = "public"
	}
	return context.WithValue(ctx, tenantContextKey{}, tenant)
}

func tenantFromContext(ctx context.Context) string {
	if tenant, ok := ctx.Value(tenantContextKey{}).(string); ok && tenant != "" {
		return tenant
	}
	return "public"
}
