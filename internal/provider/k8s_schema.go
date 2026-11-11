package provider

import (
	providerschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// k8sConnModel is the provider's kubernetes block: how the cluster is reached, named as the helm
// provider's kubernetes block names it. A provider's configuration is read at every run and never
// written to state, so its credentials are not either.
type k8sConnModel struct {
	Host                 types.String  `tfsdk:"host"`
	Token                types.String  `tfsdk:"token"`
	ClusterCACertificate types.String  `tfsdk:"cluster_ca_certificate"`
	ClientCertificate    types.String  `tfsdk:"client_certificate"`
	ClientKey            types.String  `tfsdk:"client_key"`
	Insecure             types.Bool    `tfsdk:"insecure"`
	TLSServerName        types.String  `tfsdk:"tls_server_name"`
	ProxyURL             types.String  `tfsdk:"proxy_url"`
	ConfigPath           types.String  `tfsdk:"config_path"`
	ConfigContext        types.String  `tfsdk:"config_context"`
	Exec                 *k8sExecModel `tfsdk:"exec"`
}

// k8sExecModel is a client-go credential plugin, as the helm provider's exec block names it.
type k8sExecModel struct {
	APIVersion types.String `tfsdk:"api_version"`
	Command    types.String `tfsdk:"command"`
	Args       types.List   `tfsdk:"args"`
	Env        types.Map    `tfsdk:"env"`
}

// k8sModel is the resource's kubernetes block: the release's namespace and name.
type k8sModel struct {
	Namespace      types.String `tfsdk:"namespace"`
	Release        types.String `tfsdk:"release"`
	ImagePullLogin types.Bool   `tfsdk:"image_pull_login"`
}

func k8sBlock() schema.SingleNestedBlock {
	return schema.SingleNestedBlock{
		MarkdownDescription: "The Kubernetes shape: the installer runs on the machine running OpenTofu (`<name>.sh k8s install|upgrade|rollback|uninstall`), " +
			"driving its `helm` and `kubectl` against the cluster the provider's `kubernetes` block reaches (one cluster per provider; an alias per cluster).",
		Attributes: map[string]schema.Attribute{
			"namespace": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The release's namespace; required in this block. First-install only."},
			"release": schema.StringAttribute{Optional: true,
				MarkdownDescription: "The helm release's name; left out, the project's name (the installer's default). First-install only."},
			"image_pull_login": schema.BoolAttribute{Optional: true,
				MarkdownDescription: "Writes the cluster's pull Secret for the mirror from the provider's pull account (`k8s install|upgrade --image-pull-login`), at every install and upgrade, so an authenticated mirror serves the cluster's nodes. " +
					"Opt-in: the account's credential is then readable by whoever reads the release's Secrets. Needs the provider's `username`, `password` and `registry` (the mirror; the release's own registry is refused by the installer)."},
		},
	}
}

func k8sConnBlock() providerschema.SingleNestedBlock {
	return providerschema.SingleNestedBlock{
		MarkdownDescription: "The cluster the `kubernetes` targets reach, named as the helm provider's `kubernetes` block. " +
			"With `host`, a kubeconfig of these values is written for each run (0600, in a private directory removed afterwards); " +
			"with `config_path`, that file is used; with neither, or without this block, kubectl's default (`KUBECONFIG`, else `~/.kube/config`). " +
			"A provider's configuration is read at every run and never written to state.",
		Attributes: map[string]providerschema.Attribute{
			"host": providerschema.StringAttribute{Optional: true,
				MarkdownDescription: "The API server's URL (`https://…`). Refused with `config_path` and `config_context`."},
			"token": providerschema.StringAttribute{Optional: true, Sensitive: true,
				MarkdownDescription: "A bearer token, with `host`."},
			"cluster_ca_certificate": providerschema.StringAttribute{Optional: true,
				MarkdownDescription: "PEM of the API server's certificate authority, with `host`."},
			"client_certificate": providerschema.StringAttribute{Optional: true,
				MarkdownDescription: "PEM of a client certificate, with `host` and `client_key`."},
			"client_key": providerschema.StringAttribute{Optional: true, Sensitive: true,
				MarkdownDescription: "PEM of the client certificate's key, with `client_certificate`."},
			"insecure": providerschema.BoolAttribute{Optional: true,
				MarkdownDescription: "Skips the API server certificate's check, with `host`; refused with `cluster_ca_certificate`."},
			"tls_server_name": providerschema.StringAttribute{Optional: true,
				MarkdownDescription: "The server name the API server's certificate is checked against, with `host`."},
			"proxy_url": providerschema.StringAttribute{Optional: true,
				MarkdownDescription: "A proxy to the API server (`http`, `https` or `socks5`), with `host`."},
			"config_path": providerschema.StringAttribute{Optional: true,
				MarkdownDescription: "A kubeconfig file on the machine running OpenTofu (a leading `~/` is the home directory). Refused with `host`."},
			"config_context": providerschema.StringAttribute{Optional: true,
				MarkdownDescription: "The kubeconfig's context (`--kube-context`): of `config_path`'s file, or of kubectl's default. Refused with `host`."},
		},
		Blocks: map[string]providerschema.Block{
			"exec": providerschema.SingleNestedBlock{
				MarkdownDescription: "A credential plugin kubectl and helm run for a token (`aws eks get-token`, `gke-gcloud-auth-plugin`), with `host`; never interactive.",
				Attributes: map[string]providerschema.Attribute{
					"api_version": providerschema.StringAttribute{Optional: true, MarkdownDescription: "The plugin's API version, e.g. `client.authentication.k8s.io/v1`; required in this block."},
					"command":     providerschema.StringAttribute{Optional: true, MarkdownDescription: "The plugin's command; required in this block."},
					"args":        providerschema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Its arguments."},
					"env":         providerschema.MapAttribute{Optional: true, Sensitive: true, ElementType: types.StringType, MarkdownDescription: "Environment variables it is given (sensitive)."},
				},
			},
		},
	}
}
