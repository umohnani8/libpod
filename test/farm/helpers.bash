# -*- bash -*-

load ../system/helpers.bash

function setup(){
    # only set up the podman farm before the first test
    if [[ "$BATS_TEST_NUMBER" -eq 1 ]]; then
        # rootless_user=$(cat /tmp/rootles-user)
        run podman system connection add --identity /home/$ROOTLESS_USER/.ssh/id_rsa test-node $ROOTLESS_USER@localhost
        run podman farm create test-farm test-node
    fi
    basic_setup
}

function teardown(){
    # only delete the minikube cluster if we are done with the last test
    # the $DEBUG_MINIKUBE env can be set to preserve the cluster to debug if needed
    if [[ "$BATS_TEST_NUMBER" -eq ${#BATS_TEST_NAMES[@]} ]] && [[ "$DEBUG_MINIKUBE" == "" ]]; then
        run podman farm rm --all
    fi
    basic_teardown
}
