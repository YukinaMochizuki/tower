package docker

import "testing"

// TestAUserIsLookedUpInTheContainersFiles: a user an image names is read
// from its /etc/passwd and a group from its /etc/group; a number is a
// number.
func TestAUserIsLookedUpInTheContainersFiles(t *testing.T) {
	passwd := []byte("root:x:0:0:root:/root:/bin/bash\nnode:x:1000:1000::/home/node:/bin/bash\nvscode:x:1001:1002::/home/vscode:/bin/bash\n")
	groups := []byte("root:x:0:\nnode:x:1000:\ndocker:x:999:vscode\n")
	if uid, gid, ok := lookupEntry(passwd, "vscode", 2, 3); !ok || uid != 1001 || gid != 1002 {
		t.Errorf("vscode is %d:%d, %v", uid, gid, ok)
	}
	if _, _, ok := lookupEntry(passwd, "vs", 2, 3); ok {
		t.Error("a prefix of a name was found")
	}
	if gid, _, ok := lookupEntry(groups, "docker", 2, 2); !ok || gid != 999 {
		t.Errorf("docker is %d, %v", gid, ok)
	}
	if uid, gid, ok := numericUser("1000:999"); !ok || uid != 1000 || gid != 999 {
		t.Errorf("1000:999 is %d:%d, %v", uid, gid, ok)
	}
	if _, _, ok := numericUser("node"); ok {
		t.Error("a name was read as a number")
	}
}
