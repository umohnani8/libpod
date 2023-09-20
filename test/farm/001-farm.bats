#!/usr/bin/env bats
#
# Tests of podman farm commands
#

load helpers.bash

###############################################################################
# BEGIN tests

fname="test-farm"
# rootless_user=$(cat /tmp/rootles-user)
# rootless_user=$ROOTLESS_USER

@test "farm - check farm has been created" {
    run_podman farm ls
    assert "$status" -eq 0 "get list of farms"
    assert "$output" =~ $fname
    assert "$output" =~ "test-node"
    echo "==========="
    echo $ROOTLESS_USER
}

@test "farm - build on local only" {
    iname="test-image-1"
    empty_farm="empty-farm"
    # create an empty farm
    run_podman farm create $empty_farm
    assert "$status" -eq 0 "create empty farm"
    run_podman farm --farm $empty_farm build -t $iname .
    assert "$status" -eq 0 "create new manifest list $iname"
    assert "$output" =~ "Local builder ready"
    # inspect manifest list built and saved in dir
    run_podman manifest inspect $iname
    assert "$status" -eq 0 "inspect manifest list $iname"
    assert "$output" =~ "amd64"
    # cleanup images on node
    ssh $ROOTLESS_USER@localhost podman rmi -af
    assert "$status" -eq 0 "remove built image on remote node"
}

@test "farm - build on farm node and local" {
    iname="test-image-2"
    run_podman farm build -t $iname .
    assert "$status" -eq 0 "create new manifest list $iname"
    assert "$output" =~ "Farm "$fname" ready"
    # inspect manifest list built and saved in dir
    run_podman manifest inspect $iname
    assert "$status" -eq 0 "inspect manifest list $iname"
    assert "$output" =~ "amd64"
    # cleanup images on node
    ssh $ROOTLESS_USER@localhost podman rmi -af
    assert "$status" -eq 0 "remove built image on remote node"
}

@test "farm - build on farm node only with --cleanup && --local=false" {
    iname="test-image-3"
    run_podman farm build --cleanup --local=false -t $iname .
    assert "$status" -eq 0 "create new manifest list $iname"
    assert "$output" =~ "Farm "$fname" ready"
    # inspect manifest list built and saved in dir
    cat $iname/manifest.json
    assert "$status" -eq 0 "inspect manifest list $iname"
    assert "$output" =~ "amd64"
    # see if we can ssh into node to check the image was cleaned up
    ssh $ROOTLESS_USER@localhost podman images --filter dangling=true --noheading | wc -l
    assert "$status" -eq 0 "check built image is cleaned up on remote node"
    assert "$output" =~ "0"
}

