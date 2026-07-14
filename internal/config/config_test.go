package config

import "testing"

const strongSecret = "a-very-long-random-production-secret-0123456789"

func baseProd() *Config {
	return &Config{
		AppEnv:             "production",
		JWTSecret:          strongSecret,
		CORSAllowedOrigins: []string{"https://app.example.com"},
		SeedOnStart:        true,
		SeedDemoUsers:      false,
		DemoPassword:       DefaultDemoPassword,
	}
}

func TestValidate_Prod_DefaultJWTSecret_Fails(t *testing.T) {
	c := baseProd()
	c.JWTSecret = DefaultJWTSecret
	if err := c.Validate(); err == nil {
		t.Fatal("expected production to reject the default JWT secret")
	}
}

func TestValidate_Prod_ShortJWTSecret_Fails(t *testing.T) {
	c := baseProd()
	c.JWTSecret = "too-short"
	if err := c.Validate(); err == nil {
		t.Fatal("expected production to reject a short JWT secret")
	}
}

func TestValidate_Prod_DemoUsersWithDefaultPassword_Fails(t *testing.T) {
	c := baseProd()
	c.SeedDemoUsers = true
	c.DemoPassword = DefaultDemoPassword
	if err := c.Validate(); err == nil {
		t.Fatal("expected production to reject demo users with the default password")
	}
}

func TestValidate_Prod_Secure_OK(t *testing.T) {
	c := baseProd() // strong secret, demo users off
	if err := c.Validate(); err != nil {
		t.Fatalf("secure production config should pass, got: %v", err)
	}
}

func TestValidate_NonDevEnv_TreatedAsStrict(t *testing.T) {
	// A mislabeled prod deploy ("staging"/"prod"/…) must still get the hard checks,
	// not a silent warning — guards against the fail-open gap.
	for _, env := range []string{"staging", "prod", "live", "uat"} {
		c := baseProd()
		c.AppEnv = env
		c.JWTSecret = DefaultJWTSecret
		if err := c.Validate(); err == nil {
			t.Fatalf("env %q with default JWT secret should fail validation", env)
		}
	}
}

func TestValidate_Dev_DefaultsOnly_Warns_NoError(t *testing.T) {
	c := &Config{
		AppEnv:             "development",
		JWTSecret:          DefaultJWTSecret,
		CORSAllowedOrigins: []string{"*"},
		SeedOnStart:        true,
		SeedDemoUsers:      true,
		DemoPassword:       DefaultDemoPassword,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("dev should only warn, not error, got: %v", err)
	}
}
