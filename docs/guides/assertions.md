# Write your own rules

Some rules only make sense in your repository: a naming scheme, a label every
namespace must carry, a blue/green convention. Write them as
[CEL](https://cel.dev) expressions in `.fluxlint.yaml`. fluxlint evaluates each one
against every rendered object it matches, including objects that come from charts and
from other repositories.

## An example

Two Deployments, `shop-blue` and `shop-green`, and a variable `shop_live` that names
the one taking traffic. The live colour must never be scaled to zero:

```yaml
assertions:
  - name: live colour serves traffic
    match: {kind: Deployment, namespace: shop, name: "shop-*"}
    expr: '!object.metadata.name.endsWith("-" + vars.shop_live) || object.spec.replicas > 0'
    message: the colour named by shop_live is scaled to zero
    mustMatch: true
```

```
  error   FL-A001 assertion-failed  flux-system/shop Deployment/shop/shop-green: live colour serves traffic: the colour named by shop_live is scaled to zero
            at apps/shop/green.yaml:1
```

## Fields

| Field | Meaning |
| --- | --- |
| `name` | shown in the finding |
| `match` | which objects to check: `group`, `kind`, `namespace`, `name` and `component`. All are optional and all take globs. |
| `expr` | a CEL expression that must be true |
| `message` | what to tell the reader when it is false |
| `severity` | `error` (the default), `warning` or `info` |
| `mustMatch` | fail when nothing matches, so a rename cannot switch the rule off unnoticed |

## What an expression can see

| Name | Value |
| --- | --- |
| `object` | the rendered object, after patches and substitution |
| `component` | the key of the Kustomization or HelmRelease that owns it, such as `flux-system/apps` |
| `vars` | the post-build variables that apply to the object. For an object in a nested Kustomization, these are the variables of the ancestor that substituted its spec. |

An expression that does not compile or cannot be evaluated is reported as `FL-A002`. A
typo in a field name does not
pass quietly.

## More examples

Every namespace declares a Pod Security level:

```yaml
  - name: namespaces set pod security
    match: {kind: Namespace}
    expr: 'has(object.metadata.labels) && "pod-security.kubernetes.io/enforce" in object.metadata.labels'
    message: add a pod-security.kubernetes.io/enforce label
```

No image uses `latest`:

```yaml
  - name: images are pinned
    match: {kind: Deployment}
    expr: 'object.spec.template.spec.containers.all(c, !c.image.endsWith(":latest") && c.image.contains(":"))'
    message: pin every container image to a tag or digest
```
