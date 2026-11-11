// Command seed fills a SiteTrack database with realistic data at the volume the
// brief asks for.
//
//	go run ./cmd/seed --reset                  # full scale
//	go run ./cmd/seed --reset --scale 0.01     # quick smoke test
//
// Every seeded account shares the password "Password123!". superadmin@sitetrack.test
// is a SuperAdmin and storemanager@sitetrack.test is a company store manager, so a
// reviewer can reach every screen immediately.
//
// Rows are written with GORM's CreateInBatches. The closure table is the one
// exception: it is built with a set based statement because it is a graph
// computation, not a row insert.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dongowu/sitetrack/internal/auth"
	"github.com/dongowu/sitetrack/internal/config"
	"github.com/dongowu/sitetrack/internal/database"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func main() {
	var (
		reset = flag.Bool("reset", false, "truncate every table before loading")
		scale = flag.Float64("scale", 1, "multiplier for the data volume (0.01 is a quick smoke test)")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	if err := database.Migrate(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	s := &seeder{db: db, scale: *scale, rnd: mrand.New(mrand.NewSource(20261106))}
	if *reset {
		if err := s.reset(ctx); err != nil {
			log.Fatalf("reset: %v", err)
		}
	}
	if err := s.run(ctx); err != nil {
		log.Fatalf("seed: %v", err)
	}
}

type seeder struct {
	db    *gorm.DB
	scale float64
	rnd   *mrand.Rand

	users     []database.User
	materials []database.Material
	projects  []project

	companyStore uuid.UUID
}

type project struct {
	database.Project
	StoreID   uuid.UUID
	TaskCount int
	Members   []uuid.UUID
	Stocked   []int
}

func (s *seeder) n(base int) int {
	v := int(float64(base) * s.scale)
	if v < 1 {
		v = 1
	}
	return v
}

func (s *seeder) reset(ctx context.Context) error {
	log.Println("truncating")
	if err := s.db.WithContext(ctx).Exec(`
		TRUNCATE delivery_lines, deliveries, store_stock, stores, task_closure, task_deps,
		         tasks, invitations, project_members, projects, materials, sessions, users
		RESTART IDENTITY CASCADE`).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).Exec(`ALTER SEQUENCE project_seq RESTART WITH 1`).Error; err != nil {
		return err
	}
	return database.EnsureCompanyStore(ctx, s.db)
}

func (s *seeder) run(ctx context.Context) error {
	started := time.Now()
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"users", s.seedUsers},
		{"materials", s.seedMaterials},
		{"projects", s.seedProjects},
		{"members and invitations", s.seedMembersAndInvitations},
		{"tasks and dependencies", s.seedTasks},
		{"stock", s.seedStock},
		{"deliveries", s.seedDeliveries},
	}
	for _, step := range steps {
		at := time.Now()
		log.Printf("-> %s", step.name)
		if err := step.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
		log.Printf("   done in %s", time.Since(at).Round(time.Millisecond))
	}
	log.Printf("seed complete in %s", time.Since(started).Round(time.Second))
	return nil
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

func (s *seeder) seedUsers(ctx context.Context) error {
	n := s.n(5000)

	// bcrypt is deliberately slow, so a small pool of hashes is reused: every
	// seeded account has the same password anyway.
	poolSize := runtime.NumCPU() * 4
	if poolSize > n {
		poolSize = n
	}
	hashes := make([]string, poolSize)
	var wg sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := auth.HashPassword("Password123!", 10)
			if err != nil {
				panic(err)
			}
			hashes[i] = h
		}(i)
	}
	wg.Wait()

	first := []string{"Aisha", "Ben", "Chen", "Diego", "Elif", "Farid", "Grace", "Hugo", "Ines", "Jonas",
		"Keiko", "Liam", "Maya", "Noor", "Omar", "Priya", "Quinn", "Rosa", "Samir", "Tara",
		"Umar", "Vera", "Wei", "Xin", "Yara", "Zane", "Amara", "Bruno", "Clara", "Dev"}
	last := []string{"Abbas", "Bello", "Costa", "Duarte", "Eriksen", "Fischer", "Gomez", "Haddad", "Ibrahim",
		"Jensen", "Kaur", "Larsen", "Mensah", "Novak", "Okafor", "Petrov", "Quintero", "Rossi",
		"Silva", "Tanaka", "Ueda", "Varga", "Wang", "Xu", "Yilmaz", "Zhang"}

	s.users = make([]database.User, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		switch {
		case i < 3:
			role = "superadmin"
		case i < 3+n*4/100:
			role = "store_manager"
		}
		email := fmt.Sprintf("user%d@sitetrack.test", i+1)
		switch i {
		case 0:
			email = "superadmin@sitetrack.test"
		case 3:
			email = "storemanager@sitetrack.test"
		}
		s.users = append(s.users, database.User{
			ID:           uuid.New(),
			Name:         fmt.Sprintf("%s %s", first[i%len(first)], last[(i/len(first))%len(last)]),
			Email:        email,
			PasswordHash: hashes[i%poolSize],
			AppRole:      role,
			CreatedAt:    time.Now().UTC(),
		})
	}

	return s.db.WithContext(ctx).CreateInBatches(&s.users, 500).Error
}

// ---------------------------------------------------------------------------
// materials
// ---------------------------------------------------------------------------

func (s *seeder) seedMaterials(ctx context.Context) error {
	n := s.n(20000)

	type spec struct {
		category string
		unit     string
		format   string
		text     [][]string
		number   [][]int
	}
	specs := []spec{
		{"Cement", "bag", "Cement, OPC %s %dkg bag", [][]string{{"33", "42.5", "52.5"}}, [][]int{{25, 50}}},
		{"Cement", "bag", "Cement, PPC %s %dkg bag", [][]string{{"32", "42.5", "53"}}, [][]int{{25, 50}}},
		{"Aggregate", "m3", "Aggregate, %s %dmm", [][]string{{"crushed stone", "washed sand", "pit run", "recycled"}}, [][]int{{5, 10, 20, 40}}},
		{"Steel", "ton", "Rebar, %s %dmm", [][]string{{"B500B", "B420", "A615"}}, [][]int{{8, 10, 12, 16, 20, 25, 32}}},
		{"Steel", "ton", "Steel section, %s %dmm", [][]string{{"IPE", "HEA", "UB", "UC"}}, [][]int{{100, 140, 180, 220, 300}}},
		{"Timber", "lm", "Timber, %s %dx%dmm", [][]string{{"pine", "oak", "plywood", "MDF"}}, [][]int{{50, 75, 100}, {100, 150, 200}}},
		{"Electrical", "roll", "Cable, %s %dcore %dmm2", [][]string{{"NYM", "NYY", "armoured"}}, [][]int{{2, 3, 4, 5}, {1, 2, 4, 6, 10, 16}}},
		{"Plumbing", "lm", "Pipe, %s %dmm", [][]string{{"PVC", "PPR", "copper", "HDPE", "galvanised"}}, [][]int{{15, 20, 25, 32, 40, 50, 75, 110}}},
		{"Paint", "litre", "Paint, %s %s", [][]string{{"emulsion", "gloss", "primer", "anti-corrosion", "floor"}, {"white", "magnolia", "grey", "black", "sand"}}, nil},
		{"Insulation", "m2", "Insulation, %s %dmm", [][]string{{"mineral wool", "PIR board", "EPS", "acoustic mat"}}, [][]int{{50, 75, 100, 120, 150}}},
		{"Fasteners", "each", "Fastener, %s M%d", [][]string{{"hex bolt", "anchor", "screw", "washer", "nut"}}, [][]int{{6, 8, 10, 12, 16, 20}}},
		{"Safety", "each", "Safety, %s %s", [][]string{{"helmet", "harness", "gloves", "vest", "goggles", "boots"}, {"class A", "class B", "heavy duty", "standard"}}, nil},
	}

	s.materials = make([]database.Material, 0, n)
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		sp := specs[i%len(specs)]
		base := buildMaterialName(sp.format, sp.text, sp.number, i)
		name := base
		for k := 2; seen[strings.ToLower(name)]; k++ {
			name = fmt.Sprintf("%s - variant %d", base, k)
		}
		seen[strings.ToLower(name)] = true

		s.materials = append(s.materials, database.Material{
			ID: uuid.New(), Name: name, Category: sp.category, Unit: sp.unit, CreatedAt: time.Now().UTC(),
		})
	}

	return s.db.WithContext(ctx).CreateInBatches(&s.materials, 1000).Error
}

// buildMaterialName walks the format string and fills each placeholder from the
// matching list, stepping through the combinations so names stay distinct.
func buildMaterialName(format string, text [][]string, number [][]int, i int) string {
	args := []any{}
	rest := format
	ti, ni := 0, 0
	for {
		si := strings.Index(rest, "%s")
		di := strings.Index(rest, "%d")
		if si < 0 && di < 0 {
			break
		}
		if si >= 0 && (di < 0 || si < di) {
			list := text[ti%len(text)]
			args = append(args, list[(i/(7*(ti+1)))%len(list)])
			ti++
			rest = rest[si+2:]
			continue
		}
		list := number[ni%len(number)]
		args = append(args, list[(i/(7*(ni+2)))%len(list)])
		ni++
		rest = rest[di+2:]
	}
	return fmt.Sprintf(format, args...)
}

// ---------------------------------------------------------------------------
// projects + stores
// ---------------------------------------------------------------------------

func (s *seeder) seedProjects(ctx context.Context) error {
	n := s.n(2000)

	site := []string{"Riverside", "Northgate", "Harbour Point", "Elm Street", "Kingsway", "Millbrook",
		"Westfield", "Parkview", "Granite Hill", "Lakeside", "Ashford", "Brickyard", "Sunnybank",
		"Cedar Grove", "Ironworks", "Seabreeze", "Willow Court", "Marlow", "Highbridge", "Fairview"}
	kind := []string{"Apartments", "Office Block", "Warehouse", "School", "Clinic", "Car Park",
		"Retail Unit", "Hotel", "Data Centre", "Sports Hall", "Water Treatment Works", "Road Upgrade"}

	now := time.Now().UTC()
	s.projects = make([]project, 0, n)
	rows := make([]database.Project, 0, n)
	stores := make([]database.Store, 0, n)

	for i := 0; i < n; i++ {
		start := now.AddDate(0, 0, -s.rnd.Intn(1100))
		months := 6 + s.rnd.Intn(24)
		end := start.AddDate(0, months, s.rnd.Intn(28))
		storeID := uuid.New()
		p := database.Project{
			ID:           uuid.New(),
			Seq:          i + 1,
			Code:         fmt.Sprintf("PRJ-%04d", i+1),
			Name:         fmt.Sprintf("%s %s %d", site[i%len(site)], kind[(i/len(site))%len(kind)], 1+i/40),
			PlannedStart: start,
			PlannedEnd:   end,
			CreatedBy:    s.users[s.rnd.Intn(len(s.users))].ID,
			CreatedAt:    now,
		}
		s.projects = append(s.projects, project{Project: p, StoreID: storeID})
		rows = append(rows, p)
		stores = append(stores, database.Store{
			ID: storeID, Kind: "project", ProjectID: &s.projects[i].ID, CreatedAt: now,
		})
	}

	if err := s.db.WithContext(ctx).CreateInBatches(&rows, 500).Error; err != nil {
		return err
	}
	if err := s.db.WithContext(ctx).CreateInBatches(&stores, 500).Error; err != nil {
		return err
	}

	var companyStoreID string
	if err := s.db.WithContext(ctx).Table("stores").Select("id").
		Where("kind = 'company'").Row().Scan(&companyStoreID); err != nil {
		return err
	}
	companyStore, err := uuid.Parse(companyStoreID)
	if err != nil {
		return err
	}
	s.companyStore = companyStore
	return s.db.WithContext(ctx).Exec(`SELECT setval('project_seq', ?)`, len(s.projects)).Error
}

func (s *seeder) seedMembersAndInvitations(ctx context.Context) error {
	members := []database.ProjectMember{}
	now := time.Now().UTC()

	for i := range s.projects {
		p := &s.projects[i]
		want := 1 + s.rnd.Intn(50)
		if want > len(s.users) {
			want = len(s.users)
		}
		chosen := map[uuid.UUID]bool{}
		admins := 1 + s.rnd.Intn(3)
		for len(chosen) < want {
			u := s.users[s.rnd.Intn(len(s.users))]
			if chosen[u.ID] {
				continue
			}
			chosen[u.ID] = true
			members = append(members, database.ProjectMember{
				ProjectID: p.ID, UserID: u.ID, IsAdmin: len(chosen) <= admins, JoinedAt: now,
			})
			p.Members = append(p.Members, u.ID)
		}
	}

	if err := s.db.WithContext(ctx).CreateInBatches(&members, 2000).Error; err != nil {
		return err
	}

	// 50k invitations, one open invitation per person per project.
	want := s.n(50000)
	invites := make([]database.Invitation, 0, want)
	used := map[[2]uuid.UUID]bool{}
	statuses := []string{"pending", "pending", "pending", "accepted", "rejected", "withdrawn"}
	for attempts := 0; len(invites) < want && attempts < want*100; attempts++ {
		p := &s.projects[s.rnd.Intn(len(s.projects))]
		if len(p.Members) == 0 {
			continue
		}
		invitee := s.users[s.rnd.Intn(len(s.users))].ID
		key := [2]uuid.UUID{p.ID, invitee}
		if used[key] {
			continue
		}
		used[key] = true

		status := statuses[s.rnd.Intn(len(statuses))]
		created := now.Add(-time.Duration(s.rnd.Intn(400)) * 24 * time.Hour)
		inv := database.Invitation{
			ID:        uuid.New(),
			ProjectID: p.ID,
			InviterID: p.Members[s.rnd.Intn(len(p.Members))],
			InviteeID: invitee,
			Status:    status,
			CreatedAt: created,
		}
		if status != "pending" {
			responded := created.Add(time.Duration(1+s.rnd.Intn(72)) * time.Hour)
			inv.RespondedAt = &responded
		}
		invites = append(invites, inv)
	}

	return s.db.WithContext(ctx).CreateInBatches(&invites, 2000).Error
}

// ---------------------------------------------------------------------------
// tasks and dependencies
// ---------------------------------------------------------------------------

var taskVerbs = []string{"Pour", "Erect", "Install", "Excavate", "Backfill", "Waterproof", "Render",
	"Tile", "Wire", "Plumb", "Paint", "Inspect", "Commission", "Strip", "Level", "Weld", "Glaze",
	"Test", "Dismantle", "Hand over"}

var taskObjects = []string{"level 1 slab", "level 2 slab", "foundation footing", "retaining wall",
	"roof trusses", "blockwork", "drywall partitions", "staircase", "facade cladding", "drainage run",
	"electrical riser", "switchboard", "HVAC ductwork", "sprinkler main", "lift shaft", "car park deck",
	"landscaping", "boundary wall", "water tank", "fire doors", "service corridor", "plant room"}

func (s *seeder) seedTasks(ctx context.Context) error {
	total := s.n(500000)
	big := 5 // a handful of large projects, as the brief allows up to 3,000 tasks

	remaining := total
	for i := range s.projects {
		count := 180 + s.rnd.Intn(140)
		if i < big && len(s.projects) > big {
			count = 3000
		}
		if count > remaining {
			count = remaining
		}
		s.projects[i].TaskCount = count
		remaining -= count
		if remaining <= 0 {
			break
		}
	}

	const blockSize = 8
	now := time.Now().UTC()
	written := 0

	taskBatch := make([]database.Task, 0, 4000)
	depBatch := make([]database.TaskDep, 0, 40000)

	flushTasks := func() error {
		if len(taskBatch) == 0 {
			return nil
		}
		if err := s.db.WithContext(ctx).CreateInBatches(&taskBatch, 2000).Error; err != nil {
			return err
		}
		taskBatch = taskBatch[:0]
		return nil
	}
	// Dependencies reference tasks, so the task batch has to land first even when
	// it has not reached its own threshold yet.
	flushDeps := func() error {
		if len(depBatch) == 0 {
			return nil
		}
		if err := flushTasks(); err != nil {
			return err
		}
		if err := s.db.WithContext(ctx).CreateInBatches(&depBatch, 5000).Error; err != nil {
			return err
		}
		depBatch = depBatch[:0]
		return nil
	}

	for _, p := range s.projects {
		if p.TaskCount == 0 {
			continue
		}
		tasks := make([]database.Task, p.TaskCount)
		statuses := make([]string, p.TaskCount)

		for i := 0; i < p.TaskCount; i++ {
			offset := s.rnd.Intn(1500)
			start := p.PlannedStart.AddDate(0, 0, offset)
			end := start.AddDate(0, 0, 1+s.rnd.Intn(21))

			// Statuses are assigned in dependency order so the seeded data obeys
			// the same rules the API enforces: nothing starts before what it waits
			// on is done.
			blockStart := (i / blockSize) * blockSize
			depsDone := true
			for j := blockStart; j < i; j++ {
				if statuses[j] != "done" {
					depsDone = false
					break
				}
			}
			status := "not_started"
			if depsDone {
				switch r := s.rnd.Intn(100); {
				case r < 55:
					status = "done"
				case r < 72:
					status = "in_progress"
				}
			}
			statuses[i] = status

			task := database.Task{
				ID:           uuid.New(),
				ProjectID:    p.ID,
				Seq:          i + 1,
				Code:         fmt.Sprintf("%s-T-%04d", p.Code, i+1),
				Name:         fmt.Sprintf("%s %s", taskVerbs[s.rnd.Intn(len(taskVerbs))], taskObjects[s.rnd.Intn(len(taskObjects))]),
				PlannedStart: start,
				PlannedEnd:   end,
				Status:       status,
				CreatedAt:    now,
			}
			if status != "not_started" {
				started := start.Add(time.Duration(s.rnd.Intn(24)) * time.Hour)
				if started.After(now) {
					started = now.Add(-24 * time.Hour)
				}
				task.StartedAt = &started
				if status == "done" {
					done := started.AddDate(0, 0, 1+s.rnd.Intn(14))
					if done.After(now) {
						done = now.Add(-time.Hour)
					}
					task.DoneAt = &done
				}
			}
			tasks[i] = task
		}

		// Dependencies only ever point at an earlier task inside the same block of
		// eight, so the graph is acyclic by construction and its depth stays
		// shallow enough for the closure to remain linear in the task count.
		seen := map[[2]uuid.UUID]bool{}
		for i := range tasks {
			blockStart := (i / blockSize) * blockSize
			add := func(target int) {
				if target < 0 || target >= i {
					return
				}
				key := [2]uuid.UUID{tasks[i].ID, tasks[target].ID}
				if seen[key] {
					return
				}
				seen[key] = true
				depBatch = append(depBatch, database.TaskDep{
					ProjectID: p.ID, TaskID: tasks[i].ID, DependsOnID: tasks[target].ID, CreatedAt: now,
				})
			}
			if i > blockStart {
				add(i - 1)
			}
			if i-blockStart >= 3 && s.rnd.Intn(100) < 55 {
				add(i - 3)
			}
			if i-blockStart >= 2 && s.rnd.Intn(100) < 45 {
				add(blockStart + s.rnd.Intn(i-blockStart))
			}
		}

		taskBatch = append(taskBatch, tasks...)
		written += len(tasks)
		if len(taskBatch) >= 4000 {
			if err := flushTasks(); err != nil {
				return err
			}
		}
		if len(depBatch) >= 40000 {
			if err := flushDeps(); err != nil {
				return err
			}
		}
	}
	if err := flushTasks(); err != nil {
		return err
	}
	if err := flushDeps(); err != nil {
		return err
	}
	log.Printf("   %d tasks", written)

	if err := s.db.WithContext(ctx).Exec(`UPDATE projects p SET task_seq = sub.n FROM (
		SELECT project_id, max(seq) AS n FROM tasks GROUP BY project_id) sub
		WHERE p.id = sub.project_id`).Error; err != nil {
		return err
	}
	return s.buildClosure(ctx)
}

// buildClosure closes the transitive closure for the whole database with a level
// by level sweep. Doing it globally keeps the round trips to one per depth
// instead of one per project.
func (s *seeder) buildClosure(ctx context.Context) error {
	at := time.Now()
	if err := s.db.WithContext(ctx).Exec(`
		INSERT INTO task_closure (project_id, ancestor_id, descendant_id, depth)
		SELECT project_id, depends_on_id, task_id, 1 FROM task_deps
		ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`).Error; err != nil {
		return err
	}
	for depth := 2; depth <= 256; depth++ {
		result := s.db.WithContext(ctx).Exec(`
			INSERT INTO task_closure (project_id, ancestor_id, descendant_id, depth)
			SELECT c.project_id, c.ancestor_id, d.task_id, ?
			FROM task_closure c
			JOIN task_deps d ON d.depends_on_id = c.descendant_id
			WHERE c.depth = ?
			ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`, depth, depth-1)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			log.Printf("   closure closed at depth %d in %s", depth-1, time.Since(at).Round(time.Millisecond))
			return nil
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// stock
// ---------------------------------------------------------------------------

func (s *seeder) seedStock(ctx context.Context) error {
	now := time.Now().UTC()
	rows := make([]database.StoreStock, 0, len(s.materials)+len(s.projects)*250)

	// Company store: every material has a quantity, 8% of them sit at zero so the
	// stock status screen has all three states to show.
	for _, m := range s.materials {
		qty := float64(s.rnd.Intn(3000))
		if s.rnd.Intn(100) < 8 {
			qty = 0
		}
		var reorder *float64
		if s.rnd.Intn(100) < 70 {
			v := float64(50 + s.rnd.Intn(450))
			reorder = &v
		}
		var last *time.Time
		if s.rnd.Intn(100) < 65 {
			t := now.Add(-time.Duration(s.rnd.Intn(400)) * 24 * time.Hour)
			last = &t
		}
		rows = append(rows, database.StoreStock{
			StoreID: s.companyStore, MaterialID: m.ID, Quantity: qty,
			ReorderPoint: reorder, LastDeliveryAt: last, UpdatedAt: now,
		})
	}

	// Project stores hold a subset of the catalogue.
	for i := range s.projects {
		p := &s.projects[i]
		want := 120 + s.rnd.Intn(280)
		if want > len(s.materials) {
			want = len(s.materials)
		}
		chosen := map[int]bool{}
		for len(chosen) < want {
			idx := s.rnd.Intn(len(s.materials))
			if chosen[idx] {
				continue
			}
			chosen[idx] = true
			p.Stocked = append(p.Stocked, idx)

			qty := float64(s.rnd.Intn(900))
			if s.rnd.Intn(100) < 10 {
				qty = 0
			}
			var last *time.Time
			if s.rnd.Intn(100) < 60 {
				t := now.Add(-time.Duration(s.rnd.Intn(400)) * 24 * time.Hour)
				last = &t
			}
			rows = append(rows, database.StoreStock{
				StoreID: p.StoreID, MaterialID: s.materials[idx].ID, Quantity: qty,
				LastDeliveryAt: last, UpdatedAt: now,
			})
		}
		sort.Ints(p.Stocked)
	}

	if err := s.db.WithContext(ctx).CreateInBatches(&rows, 2000).Error; err != nil {
		return err
	}
	log.Printf("   %d stock rows", len(rows))
	return nil
}

// ---------------------------------------------------------------------------
// deliveries
// ---------------------------------------------------------------------------

func (s *seeder) seedDeliveries(ctx context.Context) error {
	total := s.n(400000)
	now := time.Now().UTC()

	deliveries := make([]database.Delivery, 0, 2000)
	lines := make([]database.DeliveryLine, 0, 16000)

	flush := func() error {
		if len(deliveries) == 0 {
			return nil
		}
		if err := s.db.WithContext(ctx).CreateInBatches(&deliveries, 1000).Error; err != nil {
			return err
		}
		if err := s.db.WithContext(ctx).CreateInBatches(&lines, 5000).Error; err != nil {
			return err
		}
		deliveries = deliveries[:0]
		lines = lines[:0]
		return nil
	}

	lineTotal := 0
	for i := 0; i < total; i++ {
		p := &s.projects[s.rnd.Intn(len(s.projects))]
		if len(p.Stocked) == 0 || len(p.Members) == 0 {
			continue
		}

		outbound := s.rnd.Intn(100) < 55
		from, to := p.StoreID, s.companyStore
		if !outbound {
			from, to = s.companyStore, p.StoreID
		}

		status := "delivered"
		switch r := s.rnd.Intn(100); {
		case r < 85:
			status = "delivered"
		case r < 92:
			status = "in_transit"
		case r < 96:
			status = "rejected"
		default:
			status = "cancelled"
		}

		created := now.Add(-time.Duration(s.rnd.Intn(730*24)) * time.Hour)
		d := database.Delivery{
			ID:          uuid.New(),
			ProjectID:   p.ID,
			FromStoreID: from,
			ToStoreID:   to,
			Status:      status,
			CreatedBy:   p.Members[s.rnd.Intn(len(p.Members))],
			CreatedAt:   created,
		}
		if status != "in_transit" {
			resolved := created.Add(time.Duration(4+s.rnd.Intn(240)) * time.Hour)
			who := p.Members[s.rnd.Intn(len(p.Members))]
			d.ResolvedAt = &resolved
			d.ResolvedBy = &who
		}
		deliveries = append(deliveries, d)

		// 2 to 13 distinct materials per delivery, ~7.5 on average.
		nLines := 2 + s.rnd.Intn(12)
		used := map[int]bool{}
		for len(used) < nLines && len(used) < len(p.Stocked) {
			idx := p.Stocked[s.rnd.Intn(len(p.Stocked))]
			if used[idx] {
				continue
			}
			used[idx] = true
			lines = append(lines, database.DeliveryLine{
				DeliveryID: d.ID, MaterialID: s.materials[idx].ID,
				Quantity: float64(1 + s.rnd.Intn(120)),
			})
		}
		lineTotal += len(used)

		if len(deliveries) >= 2000 || len(lines) >= 16000 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	log.Printf("   %d deliveries, %d lines", total, lineTotal)
	return nil
}
