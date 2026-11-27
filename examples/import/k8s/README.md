# Kubernetes → deploy.yaml examples

Each directory holds a public Kubernetes manifest set (`source/`, copied
unmodified apart from dropped READMEs and scripts), where it came from
(`case.yaml`), and the `deploy.yaml` that

```bash
sklp init -from k8s -path <case>/source/<path> -stack prod
```

writes from it. `go test ./internal/importer -run TestK8sExamples` keeps
every `deploy.yaml` in sync with the importer and checks that `sklp deploy`
accepts it; `-update` regenerates them.

| Example | Upstream | Path | Manual actions | License |
|---|---|---|---|---|
| [`cnpg-cluster`](cnpg-cluster/deploy.yaml) | [cloudnative-pg/cloudnative-pg](https://github.com/cloudnative-pg/cloudnative-pg/blob/main/docs/src/samples/cluster-example-full.yaml) | `.` | 1 | Apache-2.0 |
| [`istio-bookinfo`](istio-bookinfo/deploy.yaml) | [istio/istio](https://github.com/istio/istio/blob/master/samples/bookinfo/platform/kube/bookinfo.yaml) | `.` | 0 | Apache-2.0 |
| [`k8s-guestbook`](k8s-guestbook/deploy.yaml) | [kubernetes/website](https://github.com/kubernetes/website/tree/main/content/en/examples/application/guestbook) | `.` | 0 | CC-BY-4.0 |
| [`k8s-wordpress-mysql`](k8s-wordpress-mysql/deploy.yaml) | [kubernetes/website](https://github.com/kubernetes/website/tree/main/content/en/examples/application/wordpress) | `.` | 3 | CC-BY-4.0 |
| [`kustomize-helloworld`](kustomize-helloworld/deploy.yaml) | [kubernetes-sigs/kustomize](https://github.com/kubernetes-sigs/kustomize/tree/master/examples/helloWorld) | `.` | 3 | Apache-2.0 |
| [`kustomize-ldap`](kustomize-ldap/deploy.yaml) | [kubernetes-sigs/kustomize](https://github.com/kubernetes-sigs/kustomize/tree/master/examples/ldap) | `overlays/production` | 2 | Apache-2.0 |
| [`kustomize-multibases`](kustomize-multibases/deploy.yaml) | [kubernetes-sigs/kustomize](https://github.com/kubernetes-sigs/kustomize/tree/master/examples/multibases) | `production` | 0 | Apache-2.0 |
| [`kustomize-mysql`](kustomize-mysql/deploy.yaml) | [kubernetes-sigs/kustomize](https://github.com/kubernetes-sigs/kustomize/tree/master/examples/mySql) | `.` | 1 | Apache-2.0 |
| [`kustomize-springboot`](kustomize-springboot/deploy.yaml) | [kubernetes-sigs/kustomize](https://github.com/kubernetes-sigs/kustomize/tree/master/examples/springboot) | `overlays/production` | 5 | Apache-2.0 |
| [`kustomize-wordpress`](kustomize-wordpress/deploy.yaml) | [kubernetes-sigs/kustomize](https://github.com/kubernetes-sigs/kustomize/tree/master/examples/wordpress) | `.` | 4 | Apache-2.0 |
| [`online-boutique-kustomize`](online-boutique-kustomize/deploy.yaml) | [GoogleCloudPlatform/microservices-demo](https://github.com/GoogleCloudPlatform/microservices-demo/tree/main/kustomize) | `.` | 3 | Apache-2.0 |
| [`online-boutique-release`](online-boutique-release/deploy.yaml) | [GoogleCloudPlatform/microservices-demo](https://github.com/GoogleCloudPlatform/microservices-demo/blob/main/release/kubernetes-manifests.yaml) | `.` | 3 | Apache-2.0 |
| [`podinfo`](podinfo/deploy.yaml) | [stefanprodan/podinfo](https://github.com/stefanprodan/podinfo/tree/master/kustomize) | `.` | 2 | Apache-2.0 |
| [`sock-shop`](sock-shop/deploy.yaml) | [microservices-demo/microservices-demo](https://github.com/microservices-demo/microservices-demo/blob/master/deploy/kubernetes/complete-demo.yaml) | `.` | 8 | Apache-2.0 |
| [`voting-app`](voting-app/deploy.yaml) | [dockersamples/example-voting-app](https://github.com/dockersamples/example-voting-app/tree/main/k8s-specifications) | `.` | 6 | Apache-2.0 |

## Licenses

The manifests under each `source/` belong to their upstream authors. The
Apache-2.0 ones are redistributed under that license (text in
[`LICENSES/Apache-2.0.txt`](LICENSES/Apache-2.0.txt)); the two from the
Kubernetes documentation are © The Kubernetes Authors, licensed
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). The generated
`deploy.yaml` files are derived from them under the same terms.
