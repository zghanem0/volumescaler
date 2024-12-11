#!/usr/bin/env bash

# Variables
REMOTE_HOST="ec2-user@50.16.80.172"
REMOTE_DIR="/home/ec2-user/my_project"
SSH_KEY="~/.ssh/personal.pem"

# Ensure the remote directory exists and set the correct permissions
ssh -i "${SSH_KEY}" "${REMOTE_HOST}" "sudo mkdir -p ${REMOTE_DIR} && sudo chown -R ec2-user:ec2-user ${REMOTE_DIR}"

# Use rsync to copy files, excluding the .git directory
rsync -avz --exclude '.git' -e "ssh -i ${SSH_KEY}" ./ "${REMOTE_HOST}:${REMOTE_DIR}"

# (Optional) Add ec2-user to the docker group if you want to eventually run docker without sudo
# ssh -i "${SSH_KEY}" "${REMOTE_HOST}" "sudo usermod -aG docker ec2-user"
# Note: This requires re-logging in before taking effect. If you rely on sudo in Makefile, this step isn’t needed.

# Run the deploy target
ssh -i "${SSH_KEY}" "${REMOTE_HOST}" "cd ${REMOTE_DIR} && make deploy"