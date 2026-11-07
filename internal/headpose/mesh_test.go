package headpose

import "testing"

func TestMeshTables(t *testing.T) {
	if len(MeshTesselation) != 1322 {
		t.Fatalf("tessellation: %d segments", len(MeshTesselation))
	}
	seen := map[MeshEdge]bool{}
	for _, e := range MeshTesselation {
		if e[0] >= e[1] || int(e[1]) >= MeshPoints {
			t.Fatalf("segment %v out of order or range", e)
		}
		if seen[e] {
			t.Fatalf("duplicate segment %v", e)
		}
		seen[e] = true
	}
	for _, f := range MeshContours {
		if len(f.Edges) == 0 {
			t.Fatalf("%s: no segments", f.Name)
		}
		for _, e := range f.Edges {
			if int(e[0]) >= NumLandmarks || int(e[1]) >= NumLandmarks {
				t.Fatalf("%s: segment %v out of range", f.Name, e)
			}
		}
	}
	// i contorni delle iridi girano attorno al loro centro
	for _, e := range meshLeftIris {
		if e[0] <= LandmarkLeftIris || e[0] > LandmarkLeftIris+4 {
			t.Fatalf("left iris segment %v", e)
		}
	}
	for _, e := range meshRightIris {
		if e[0] <= LandmarkRightIris || e[0] > LandmarkRightIris+4 {
			t.Fatalf("right iris segment %v", e)
		}
	}
}
