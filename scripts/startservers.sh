#!/bin/bash
set -e

user="ubuntu"

echo "killing servers"
while read host; do
    ssh -i 6824.pem $user@$host ". /home/$user/code/xfertest/scripts/killservers.sh" &
done < servers
wait

echo "starting servers"
i=0
while read host; do
    ssh -i 6824.pem $user@$host "nohup /home/$user/code/xfertest/src/main/xfer -me $i -hosts /home/$user/code/xfertest/scripts/servers > out" &
    ((i++))
done < servers

wait
