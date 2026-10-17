package reviewer

import "testing"

const sampleDiff = `diff --git a/greet.go b/greet.go
index abc1234..def5678 100644
--- a/greet.go
+++ b/greet.go
@@ -1,5 +1,6 @@
 package greet
 
 func Hello(name string) string {
-	return "hi " + name
+	// greet loudly
+	return "HI " + name
 }
`

func TestParseUnifiedDiff_AnnotatesAddedAndContextLinesWithNewFileLineNumbers(t *testing.T) {
	files, err := ParseUnifiedDiff([]byte(sampleDiff))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != "greet.go" {
		t.Fatalf("files = %#v, want one file named greet.go", files)
	}
	var added []Line
	for _, l := range files[0].Lines {
		if l.Kind == Added {
			added = append(added, l)
		}
	}
	if len(added) != 2 {
		t.Fatalf("added lines = %#v, want 2", added)
	}
	if added[0].NewLine != 4 || added[0].Text != "\t// greet loudly" {
		t.Fatalf("added[0] = %#v, want new line 4", added[0])
	}
	if added[1].NewLine != 5 || added[1].Text != `	return "HI " + name` {
		t.Fatalf("added[1] = %#v, want new line 5", added[1])
	}
	var removed []Line
	for _, l := range files[0].Lines {
		if l.Kind == Removed {
			removed = append(removed, l)
		}
	}
	if len(removed) != 1 || removed[0].OldLine != 4 || removed[0].NewLine != 0 {
		t.Fatalf("removed = %#v, want one removed line at old line 4 with no new line", removed)
	}
}

func TestParseUnifiedDiff_NewFileHasNoOldLineNumbers(t *testing.T) {
	diff := `diff --git a/new.go b/new.go
new file mode 100644
index 0000000..abc1234
--- /dev/null
+++ b/new.go
@@ -0,0 +1,2 @@
+package new
+
`
	files, err := ParseUnifiedDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != "new.go" {
		t.Fatalf("files = %#v, want one file named new.go", files)
	}
	if len(files[0].Lines) != 2 || files[0].Lines[0].NewLine != 1 || files[0].Lines[0].OldLine != 0 {
		t.Fatalf("lines = %#v, want two added lines starting at new line 1", files[0].Lines)
	}
	if !files[0].Added || files[0].Deleted {
		t.Fatalf("file = %#v, want it recorded as added and not deleted", files[0])
	}
}

func TestParseUnifiedDiff_DeletedFileKeepsItsPathFromTheOldSide(t *testing.T) {
	diff := `diff --git a/gone.go b/gone.go
deleted file mode 100644
index abc1234..0000000
--- a/gone.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package gone
-
`
	files, err := ParseUnifiedDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != "gone.go" {
		t.Fatalf("files = %#v, want one file named gone.go", files)
	}
	if len(files[0].Lines) != 2 || files[0].Lines[0].Kind != Removed || files[0].Lines[0].OldLine != 1 {
		t.Fatalf("lines = %#v, want two removed lines starting at old line 1", files[0].Lines)
	}
	if !files[0].Deleted || files[0].Added {
		t.Fatalf("file = %#v, want it recorded as deleted and not added", files[0])
	}
}

func TestParseUnifiedDiff_PureDeletionHunkIsNotADeletedFile(t *testing.T) {
	// Generated with no context, every line of this hunk is a removal - the
	// one case where reading added/deleted off the line kinds would call a
	// surviving file deleted.
	diff := `diff --git a/keep_test.go b/keep_test.go
index abc1234..def5678 100644
--- a/keep_test.go
+++ b/keep_test.go
@@ -4,2 +3,0 @@
-	if got != want {
-		t.Fatal("mismatch")
`
	files, err := ParseUnifiedDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	if len(files) != 1 || files[0].Added || files[0].Deleted {
		t.Fatalf("files = %#v, want one file recorded as neither added nor deleted", files)
	}
}

func TestParseUnifiedDiff_MultipleFilesAreAllCaptured(t *testing.T) {
	diff := sampleDiff + `diff --git a/other.go b/other.go
index 1111111..2222222 100644
--- a/other.go
+++ b/other.go
@@ -10,2 +10,2 @@
 context
-old
+new
`
	files, err := ParseUnifiedDiff([]byte(diff))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	if len(files) != 2 || files[0].Path != "greet.go" || files[1].Path != "other.go" {
		t.Fatalf("files = %#v, want greet.go then other.go", files)
	}
}

func TestRenderForReview_CitesNewFileLineNumbersInTheLeftColumn(t *testing.T) {
	files, err := ParseUnifiedDiff([]byte(sampleDiff))
	if err != nil {
		t.Fatalf("ParseUnifiedDiff() error = %v", err)
	}
	rendered := RenderForReview(files)
	if !contains(rendered, "FILE greet.go") {
		t.Fatalf("rendered = %q, want a FILE header", rendered)
	}
	if !contains(rendered, "     5 +") {
		t.Fatalf("rendered = %q, want the added line annotated with new line 5", rendered)
	}
	if !contains(rendered, "removed, was old line 4") {
		t.Fatalf("rendered = %q, want the removed line labeled with its old line number", rendered)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
