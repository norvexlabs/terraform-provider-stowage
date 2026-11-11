package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// The kubeconfig's names for the one cluster, user and context it holds.
const kubeconfigName = "stowage"

// kubeconfig is the kubeconfig of the provider's kubernetes block given host: one cluster, one user, one
// context, current. PEM values go in as their *-data (base64), as kubectl reads them; an exec
// plugin is never interactive (a run has no terminal).
func kubeconfig(ctx context.Context, b *k8sConnModel, diags *diag.Diagnostics) []byte {
	cluster := map[string]any{"server": b.Host.ValueString()}
	if v := b.ClusterCACertificate.ValueString(); v != "" {
		cluster["certificate-authority-data"] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	if b.Insecure.ValueBool() {
		cluster["insecure-skip-tls-verify"] = true
	}
	if v := b.TLSServerName.ValueString(); v != "" {
		cluster["tls-server-name"] = v
	}
	if v := b.ProxyURL.ValueString(); v != "" {
		cluster["proxy-url"] = v
	}
	user := map[string]any{}
	if v := b.Token.ValueString(); v != "" {
		user["token"] = v
	}
	if v := b.ClientCertificate.ValueString(); v != "" {
		user["client-certificate-data"] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	if v := b.ClientKey.ValueString(); v != "" {
		user["client-key-data"] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	if e := b.Exec; e != nil {
		args := []string{}
		if known(e.Args) {
			diags.Append(e.Args.ElementsAs(ctx, &args, false)...)
		}
		env := []map[string]string{}
		vars := stringMap(ctx, e.Env, diags)
		for _, k := range slices.Sorted(maps.Keys(vars)) {
			env = append(env, map[string]string{"name": k, "value": vars[k]})
		}
		user["exec"] = map[string]any{
			"apiVersion": e.APIVersion.ValueString(), "command": e.Command.ValueString(),
			"args": args, "env": env, "interactiveMode": "Never",
		}
	}
	out, err := json.Marshal(map[string]any{
		"apiVersion":      "v1",
		"kind":            "Config",
		"clusters":        []any{map[string]any{"name": kubeconfigName, "cluster": cluster}},
		"users":           []any{map[string]any{"name": kubeconfigName, "user": user}},
		"contexts":        []any{map[string]any{"name": kubeconfigName, "context": map[string]string{"cluster": kubeconfigName, "user": kubeconfigName}}},
		"current-context": kubeconfigName,
	})
	if err != nil {
		diags.AddError("Writing the kubeconfig", err.Error())
	}
	return out
}
