package orchestrator

import "testing"

func TestScanDangerous(t *testing.T) {
	cases := []struct {
		cmd     string
		want    bool
		ruleHas string
	}{
		{"rm -rf /tmp/x", true, "rm -rf"},
		{"rm -fr build/", true, "rm -rf"},
		{"sudo rm -rf --no-preserve-root /", true, "rm -rf"},
		{"git push --force origin main", true, "git push --force"},
		{"git push -f", true, "git push --force"},
		{"git reset --hard HEAD~3", true, "git reset --hard"},
		{"pkill node", true, "kill"},
		{"killall -9 python", true, "kill"},
		{"dd if=/dev/zero of=/dev/sda", true, "dd"},
		{"mkfs.ext4 /dev/sdb1", true, "mkfs"},
		{"chmod -R 000 /", true, "chmod -R 000"},
		{":(){ :|:& };:", true, "fork bomb"},
		{"echo hi > /dev/sda", true, "overwrite block device"},
		// Benign — must NOT match.
		{"rm file.txt", false, ""},
		{"remove()", false, ""},
		{"go test ./...", false, ""},
		{"git push origin main", false, ""},
		{"npm install", false, ""},
		{"", false, ""},
		{"cat README.md", false, ""},
	}
	for _, c := range cases {
		got, rule := scanDangerous(c.cmd)
		if got != c.want {
			t.Errorf("scanDangerous(%q) = %v, want %v (rule=%q)", c.cmd, got, c.want, rule)
			continue
		}
		if c.want && rule != c.ruleHas {
			t.Errorf("scanDangerous(%q) rule = %q, want %q", c.cmd, rule, c.ruleHas)
		}
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{`exec: "pytest": executable file not found in $PATH`, "environment"},
		{"bash: foo: command not found", "environment"},
		{"open /nonexistent: no such file or directory", "environment"},
		{"permission denied", "environment"},
		{"start: fork/exec: bad", "environment"},
		{"ENOENT: missing", "environment"},
		{"go is not installed", "environment"},
		{"FAIL: TestFoo expected 1 got 2", "logic"},
		{"the diff shows an incomplete implementation", "logic"},
		{"", "logic"},
	}
	for _, c := range cases {
		if got := classifyFailure(c.text); got != c.want {
			t.Errorf("classifyFailure(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
