# delete old netes namespaces
kubectl delete namespace openfaas openfaas-fn

# setup namespaces
kubectl apply -f https://raw.githubusercontent.com/zack-hable/faas-netes/master/namespaces.yml

# create password
export PASSWORD=$(head -c 12 /dev/urandom | shasum| cut -d' ' -f1)

# init secret
kubectl -n openfaas create secret generic basic-auth --from-literal=basic-auth-user=admin --from-literal=basic-auth-password="$PASSWORD"

# install
kubectl apply -f ./yaml

# attempt a login to start a session
sleep 30
echo -n $PASSWORD | faas-cli login --password-stdin --gateway 127.0.0.1:31112
echo "PASSWORD is $PASSWORD"
