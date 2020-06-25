# CRC Cluster Bot

## Running Locally

```
export BOT_TOKEN="<your slack classic app oauth token>"
export OPENSHIFT_PULL_SECRET"<the openshift pull secret used to configure CRC clusters>"
make run
```

## Building

```
make build
```

## Releasing

```
make release RELEASE_VERSION=0.0.5 RELEASE_REGISTRY=quay.io/bbrowning/crc-cluster-bot
```

## Deploying to a cluster

Deploy the prerequisite namespace/RBAC stuff:
```
oc apply -f deploy/namespace.yaml
oc apply -f deploy/service_account.yaml
oc apply -f deploy/role.yaml
oc apply -f deploy/role_binding.yaml
```

Then, assuming you have your Slack bot token in a `$BOT_TOKEN` environment variable and your OpenShift pull secret in a file called `pull-secret`:

```
oc create secret generic -n crc-cluster-bot crc-cluster-bot --from-literal=botToken=$BOT_TOKEN --from-file=openshiftPullSecret=pull-secret
```

Finally, deploy the bot:

```
cat deploy/deployment.yaml | sed -e "s|REPLACE_IMAGE|quay.io/bbrowning/crc-cluster-bot:v0.0.5|g" | oc apply -f -
```
